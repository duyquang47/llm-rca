package rca.payment;

import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ThreadLocalRandom;

/**
 * Java port of internal/chain/faults: same JSON spec on the same internal
 * :9999/_fault endpoint, so faultctl and every scenario work unchanged.
 *
 * Same absolute constraint as the Go package: never log, trace or meter
 * anything about this state. The class name avoids the word "fault" because
 * stack traces (e.g. OutOfMemoryError under mem_leak) are exported with logs
 * and would trip the scenarios' must_not_include check. The JDK HttpServer
 * below is kept untraced via OTEL_INSTRUMENTATION_JAVA_HTTP_SERVER_ENABLED=false
 * in the Dockerfile.
 */
final class Control {
    record Latency(double probability, int ms) {}
    record Err(double probability, int code) {}
    // snake_case components match the Go JSON tags exactly.
    record Spec(String route, Latency latency, Err error, boolean cpu_burn, boolean mem_leak,
                Integer pool_size, Boolean n_plus_one, Integer retry) {}

    private static final ObjectMapper JSON = new ObjectMapper()
            .configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);

    private final Map<String, Spec> specs = new ConcurrentHashMap<>();
    // Grows under mem_leak and is never freed. Unlike Go, the JVM hits its own
    // heap ceiling (-XX:MaxRAMPercentage) first: requests start failing with
    // OutOfMemoryError while the pod keeps running — no OOMKill.
    private final List<byte[]> leaks = Collections.synchronizedList(new ArrayList<>());

    int effectiveRetry(String callName, int configured) {
        Spec s = specs.get(callName);
        return s == null || s.retry() == null ? configured : s.retry();
    }

    /** Returns 0 to proceed, or the HTTP status the request must fail with. */
    int apply(String route) throws InterruptedException {
        Spec s = specs.get(route);
        if (s == null) return 0;
        if (s.mem_leak()) leaks.add(new byte[8 << 20]);
        if (s.cpu_burn()) {
            long end = System.nanoTime() + 200_000_000L;
            while (System.nanoTime() < end) { /* spin */ }
        }
        ThreadLocalRandom rnd = ThreadLocalRandom.current();
        if (s.latency() != null && rnd.nextDouble() < s.latency().probability()) {
            Thread.sleep(s.latency().ms());
        }
        if (s.error() != null && rnd.nextDouble() < s.error().probability()) {
            return s.error().code() != 0 ? s.error().code() : 500;
        }
        return 0;
    }

    void serve(int port) throws IOException {
        HttpServer srv = HttpServer.create(new InetSocketAddress(port), 0);
        srv.createContext("/_fault", ex -> {
            int code;
            try {
                switch (ex.getRequestMethod()) {
                    case "POST" -> {
                        Spec s = JSON.readValue(ex.getRequestBody(), Spec.class);
                        if (s.route() == null || s.route().isEmpty()) {
                            code = 400;
                        } else {
                            specs.put(s.route(), s);
                            code = 204;
                        }
                    }
                    case "DELETE" -> {
                        String route = queryParam(ex.getRequestURI().getRawQuery(), "route");
                        if (route == null || route.isEmpty()) {
                            code = 400;
                        } else {
                            specs.remove(route);
                            code = 204;
                        }
                    }
                    default -> code = 405;
                }
            } catch (IOException e) {
                code = 400;
            }
            ex.sendResponseHeaders(code, -1);
            ex.close();
        });
        srv.start();
    }

    private static String queryParam(String rawQuery, String name) {
        if (rawQuery == null) return null;
        for (String kv : rawQuery.split("&")) {
            int eq = kv.indexOf('=');
            if (eq > 0 && kv.substring(0, eq).equals(name)) {
                return URLDecoder.decode(kv.substring(eq + 1), StandardCharsets.UTF_8);
            }
        }
        return null;
    }
}
