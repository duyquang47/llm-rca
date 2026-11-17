"""Python port of internal/chain/consumer (notify-worker).

Same stream/group semantics, same log bodies, same crash-on-poison behaviour
(an uncaught exception kills the process -> CrashLoopBackOff, which the
poison_message scenario relies on). Run under `opentelemetry-instrument`,
which exports traces/logs/metrics over OTLP.
"""
import json
import logging
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

import redis
import yaml
from opentelemetry import propagate, trace

log = logging.getLogger("notify-worker")
tracer = trace.get_tracer("notify-worker-0")

# Same constraint as the Go faults package: never log/trace this state.
# shortcut: specs are accepted and stored but not applied — same as the Go
# worker, which has no HTTP routes for them to act on.
_specs = {}


class _Control(BaseHTTPRequestHandler):
    def do_POST(self):
        try:
            spec = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        except ValueError:
            return self._reply(400)
        if not spec.get("route"):
            return self._reply(400)
        _specs[spec["route"]] = spec
        self._reply(204)

    def do_DELETE(self):
        from urllib.parse import parse_qs, urlparse
        route = parse_qs(urlparse(self.path).query).get("route", [""])[0]
        if not route:
            return self._reply(400)
        _specs.pop(route, None)
        self._reply(204)

    def do_GET(self):
        self._reply(405)

    def _reply(self, code):
        self.send_response(code)
        self.end_headers()

    def log_message(self, *args):  # stdlib default prints every request to stderr
        pass


def ensure_group(cli, cfg):
    backoff = 1
    while True:
        try:
            cli.xgroup_create(cfg["stream"], cfg["group"], id="0", mkstream=True)
            return
        except redis.ResponseError as e:
            if "BUSYGROUP" in str(e):
                return
            err = e
        except redis.RedisError as e:
            err = e
        log.error("create consumer group failed, retrying", extra={"err": str(err), "backoff": f"{backoff}s"})
        time.sleep(backoff)
        backoff = min(backoff * 2, 30)


def process(cli, cfg, entry_id, fields):
    ctx = None
    if fields.get("traceparent"):
        ctx = propagate.extract({"traceparent": fields["traceparent"]})
    with tracer.start_as_current_span("notify-worker process", context=ctx):
        published_at = fields.get("published_at")
        if not published_at:
            # Uncaught on purpose: crashes the worker like the Go panic.
            raise RuntimeError(f"poison message {entry_id} on stream {cfg['stream']}: missing published_at field")
        log.info("processed notification", extra={"entry_id": entry_id, "published_at": published_at})
        try:
            cli.xack(cfg["stream"], cfg["group"], entry_id)
        except redis.RedisError as e:
            log.error("xack failed", extra={"entry_id": entry_id, "err": str(e)})


def main():
    with open(os.environ.get("CONFIG_PATH", "/etc/chain/config.yaml")) as f:
        cfg = yaml.safe_load(f)["consume"]

    threading.Thread(target=HTTPServer(("", 9999), _Control).serve_forever, daemon=True).start()

    cli = redis.Redis.from_url(f"redis://{cfg['addr']}", decode_responses=True)
    ensure_group(cli, cfg)
    log.info("notify-worker consuming", extra={"stream": cfg["stream"], "group": cfg["group"]})

    while True:
        try:
            streams = cli.xreadgroup(cfg["group"], cfg["consumer"], {cfg["stream"]: ">"}, count=10, block=5000)
        except redis.RedisError as e:
            log.error("xreadgroup failed", extra={"err": str(e)})
            if str(e).startswith("NOGROUP"):
                ensure_group(cli, cfg)
            else:
                time.sleep(1)
            continue
        for _stream, entries in streams or []:
            for entry_id, fields in entries:
                process(cli, cfg, entry_id, fields)


if __name__ == "__main__":
    # basicConfig is a no-op once the OTel log handler is attached, so wire
    # the console handler and level explicitly.
    root = logging.getLogger()
    root.setLevel(logging.INFO)
    root.addHandler(logging.StreamHandler(sys.stdout))
    main()
