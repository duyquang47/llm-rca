package rca.payment;

import jakarta.servlet.http.HttpServletRequest;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.context.event.ApplicationReadyEvent;
import org.springframework.context.event.EventListener;
import org.springframework.http.ResponseEntity;
import org.springframework.web.servlet.mvc.method.RequestMappingInfo;
import org.springframework.web.servlet.mvc.method.annotation.RequestMappingHandlerMapping;
import org.yaml.snakeyaml.Yaml;

import java.io.FileReader;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.List;
import java.util.Map;

/**
 * Java port of the "calls only" subset of cmd/chain: reads the same
 * ConfigMap (CONFIG_PATH), serves each route, calls downstreams with the
 * same retry/timeout/status semantics and the same log + response bodies.
 *
 * shortcut: only routes[].calls is supported — payment has no cache/db/publish.
 * Add them here if another service gets a Java port.
 */
@SpringBootApplication
public class App {
    private static final Logger log = LoggerFactory.getLogger(App.class);
    static final Control CONTROL = new Control();
    static List<Route> routes;

    record Call(String name, String url, Duration timeout, int retry) {}
    record Route(String path, List<Call> calls) {}

    public static void main(String[] args) throws Exception {
        Map<String, Object> cfg;
        try (var r = new FileReader(System.getenv().getOrDefault("CONFIG_PATH", "/etc/chain/config.yaml"))) {
            cfg = new Yaml().load(r);
        }
        routes = parseRoutes(cfg);
        String listen = String.valueOf(cfg.getOrDefault("listen", ":8080"));
        System.setProperty("server.port", listen.substring(listen.lastIndexOf(':') + 1));

        // Spring's startup info says 'falling back to ... "default" profile' — contains
        // "fault", which scenarios forbid in exported logs.
        System.setProperty("spring.main.log-startup-info", "false");

        CONTROL.serve(9999);
        SpringApplication.run(App.class, args);
    }

    @SuppressWarnings("unchecked")
    static List<Route> parseRoutes(Map<String, Object> cfg) {
        var raw = (List<Map<String, Object>>) cfg.getOrDefault("routes", List.of());
        return raw.stream().map(r -> new Route((String) r.get("path"),
                ((List<Map<String, Object>>) r.getOrDefault("calls", List.of())).stream()
                        .map(c -> new Call((String) c.get("name"), (String) c.get("url"),
                                goDuration(String.valueOf(c.getOrDefault("timeout", "2s"))),
                                ((Number) c.getOrDefault("retry", 0)).intValue()))
                        .toList()))
                .toList();
    }

    /** Parses the Go durations used in the configs: "500ms", "2s", "1m". */
    static Duration goDuration(String s) {
        if (s.endsWith("ms")) return Duration.ofMillis(Long.parseLong(s.substring(0, s.length() - 2)));
        if (s.endsWith("s")) return Duration.ofMillis((long) (Double.parseDouble(s.substring(0, s.length() - 1)) * 1000));
        if (s.endsWith("m")) return Duration.ofMinutes(Long.parseLong(s.substring(0, s.length() - 1)));
        throw new IllegalArgumentException("unsupported duration " + s);
    }

    @EventListener(ApplicationReadyEvent.class)
    void register(ApplicationReadyEvent ev) throws NoSuchMethodException {
        var mapping = ev.getApplicationContext()
                .getBean("requestMappingHandlerMapping", RequestMappingHandlerMapping.class);
        var m = Handler.class.getMethod("handle", HttpServletRequest.class);
        for (Route r : routes) {
            mapping.registerMapping(
                    RequestMappingInfo.paths(r.path()).options(mapping.getBuilderConfiguration()).build(),
                    new Handler(r), m);
        }
    }

    public static final class Handler {
        private static final HttpClient CLIENT = HttpClient.newHttpClient();
        private final Route route;

        Handler(Route route) { this.route = route; }

        public ResponseEntity<String> handle(HttpServletRequest req) throws InterruptedException {
            int code = CONTROL.apply(route.path());
            if (code != 0) return ResponseEntity.status(code).body("internal error\n");

            for (Call call : route.calls()) {
                String err = callDownstream(req, call);
                if (err != null) {
                    // Constant body + attributes, like Go slog, so the log template stays stable.
                    log.atError().addKeyValue("target", call.name()).addKeyValue("err", err)
                            .log("downstream call failed");
                    return ResponseEntity.status(502).body("downstream failed\n");
                }
            }
            return ResponseEntity.ok().build();
        }

        /** Returns null on success, else an error description (same wording as Go). */
        private String callDownstream(HttpServletRequest in, Call call) throws InterruptedException {
            String url = call.url() + (in.getQueryString() != null ? "?" + in.getQueryString() : "");
            int attempts = CONTROL.effectiveRetry(call.name(), call.retry()) + 1;
            String lastErr = null;
            for (int i = 0; i < attempts; i++) {
                try {
                    var req = HttpRequest.newBuilder(URI.create(url)).timeout(call.timeout())
                            .method(in.getMethod(), HttpRequest.BodyPublishers.noBody()).build();
                    var resp = CLIENT.send(req, HttpResponse.BodyHandlers.discarding());
                    if (resp.statusCode() < 400) return null;
                    lastErr = call.name() + " returned " + resp.statusCode();
                } catch (java.io.IOException e) {
                    lastErr = e.toString();
                }
            }
            return "all " + attempts + " attempts to " + call.name() + " failed: " + lastErr;
        }
    }
}
