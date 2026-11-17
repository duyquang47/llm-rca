# Build: docker build --build-arg HTTP_PROXY=... --build-arg HTTPS_PROXY=... -t quang47/java-payment-service:v1.0.1 cmd/payment-java
FROM maven:3-eclipse-temurin-21 AS build
ARG HTTP_PROXY HTTPS_PROXY
WORKDIR /src
COPY pom.xml .
COPY src ./src
# Maven ignores HTTP(S)_PROXY env; write a settings.xml proxy when one is given.
RUN set -e; if [ -n "$HTTPS_PROXY" ]; then h=${HTTPS_PROXY#*://}; h=${h%/}; \
      mkdir -p ~/.m2; printf '<settings><proxies><proxy><id>p</id><active>true</active><protocol>https</protocol><host>%s</host><port>%s</port></proxy><proxy><id>q</id><active>true</active><protocol>http</protocol><host>%s</host><port>%s</port></proxy></proxies></settings>' \
      "${h%:*}" "${h##*:}" "${h%:*}" "${h##*:}" > ~/.m2/settings.xml; fi; \
    mvn -q -B package -DskipTests

FROM eclipse-temurin:21-jre
ARG HTTP_PROXY HTTPS_PROXY
ADD https://github.com/open-telemetry/opentelemetry-java-instrumentation/releases/download/v2.15.0/opentelemetry-javaagent.jar /otel/opentelemetry-javaagent.jar
COPY --from=build /src/target/payment.jar /app/payment.jar
# JAVA_HTTP_SERVER disabled: keeps the internal :9999 control server out of traces/metrics.
ENV JAVA_TOOL_OPTIONS="-javaagent:/otel/opentelemetry-javaagent.jar -XX:MaxRAMPercentage=60" \
    OTEL_EXPORTER_OTLP_PROTOCOL=grpc \
    OTEL_METRICS_EXPORTER=otlp \
    OTEL_LOGS_EXPORTER=otlp \
    OTEL_METRIC_EXPORT_INTERVAL=15000 \
    OTEL_INSTRUMENTATION_LOGBACK_APPENDER_EXPERIMENTAL_CAPTURE_KEY_VALUE_PAIR_ATTRIBUTES=true \
    OTEL_INSTRUMENTATION_JAVA_HTTP_SERVER_ENABLED=false
EXPOSE 8080 9999
ENTRYPOINT ["java", "-jar", "/app/payment.jar"]
