#!/usr/bin/env bash
# Smoke test for the polyglot images: same contract as the Go chain service.
# Usage: cmd/smoke-polyglot.sh [tag]   (default v1.0.1; needs docker only)
set -euo pipefail
V=${1:-v1.0.1}; D=$(mktemp -d); N=smk$$
cleanup() { docker rm -f $N-mock $N-pay $N-redis $N-nw >/dev/null 2>&1; docker network rm $N >/dev/null 2>&1; rm -rf "$D"; }
trap cleanup EXIT
check() { [ "$2" = "$3" ] && echo "ok   $1" || { echo "FAIL $1: got '$2' want '$3'"; exit 1; }; }

docker network create $N >/dev/null
cat > "$D/pay.yaml" <<Y
listen: ":8080"
routes: [{path: /charge, calls: [{name: thirdparty-mock, url: "http://$N-mock:8080/pay", timeout: 2s, retry: 1}]}]
Y
cat > "$D/nw.yaml" <<Y
consume: {addr: "$N-redis:6379", stream: notifications, group: notify-worker, consumer: notify-worker-0}
Y
docker run -d --name $N-mock --network $N python:3.12-slim python -c "
import http.server,os
class H(http.server.BaseHTTPRequestHandler):
  def do_GET(s):
    s.send_response(int(open('/tmp/c').read()) if os.path.exists('/tmp/c') else 200); s.end_headers()
http.server.HTTPServer(('',8080),H).serve_forever()" >/dev/null
docker run -d --name $N-redis --network $N redis:7-alpine >/dev/null
OTEL="-e OTEL_TRACES_EXPORTER=none -e OTEL_METRICS_EXPORTER=none -e OTEL_LOGS_EXPORTER=none"
docker run -d --name $N-pay --network $N $OTEL -v "$D/pay.yaml:/etc/chain/config.yaml" quang47/java-payment-service:$V >/dev/null
docker run -d --name $N-nw --network $N $OTEL -v "$D/nw.yaml:/etc/chain/config.yaml" quang47/python-notify-service:$V >/dev/null

c() { docker exec $N-mock python -c "
import urllib.request as u,sys
r=u.Request(sys.argv[1],data=sys.argv[3].encode() if sys.argv[3] else None,method=sys.argv[2])
try: print(u.urlopen(r,timeout=10).status)
except u.HTTPError as e: print(e.code)
except Exception: print(0)" "$@"; }
for _ in $(seq 60); do [ "$(c http://$N-pay:8080/charge GET '')" = 200 ] && break; sleep 1; done

check "payment ok"            "$(c http://$N-pay:8080/charge GET '')" 200
check "control post"          "$(c http://$N-pay:9999/_fault POST '{"route":"/charge","error":{"probability":1,"code":503}}')" 204
check "injected error code"   "$(c http://$N-pay:8080/charge GET '')" 503
check "missing route"         "$(c http://$N-pay:9999/_fault POST '{}')" 400
check "control delete"        "$(c "http://$N-pay:9999/_fault?route=/charge" DELETE '')" 204
docker exec $N-mock sh -c 'echo 429 > /tmp/c'
check "429 downstream -> 502" "$(c http://$N-pay:8080/charge GET '')" 502
check "failure log"           "$(docker logs $N-pay 2>&1 | grep -c 'downstream call failed')" 1

check "worker control post"   "$(c http://$N-nw:9999/_fault POST '{"route":"/x"}')" 204
docker exec $N-redis redis-cli XADD notifications '*' published_at 1 >/dev/null
sleep 2
check "processed log"         "$(docker logs $N-nw 2>&1 | grep -c 'processed notification')" 1
docker exec $N-redis redis-cli XADD notifications '*' foo bar >/dev/null
sleep 3
check "poison crashes worker" "$(docker inspect -f '{{.State.ExitCode}}' $N-nw)" 1
check "no 'fault' in logs"  "$( (docker logs $N-pay; docker logs $N-nw) 2>&1 | grep -ci fault || true)" 0
