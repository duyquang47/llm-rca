# Build: docker build --build-arg HTTP_PROXY=... --build-arg HTTPS_PROXY=... -t quang47/python-notify-service:v1.0.1 cmd/notify-py
FROM python:3.12-slim
ARG HTTP_PROXY HTTPS_PROXY
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt && opentelemetry-bootstrap -a install
COPY app.py .
# redis instrumentation disabled: it would emit a span every 5s blocking
# XREADGROUP; the Go worker traces only "notify-worker process" too.
ENV PYTHONUNBUFFERED=1 \
    OTEL_EXPORTER_OTLP_PROTOCOL=grpc \
    OTEL_TRACES_EXPORTER=otlp \
    OTEL_METRICS_EXPORTER=otlp \
    OTEL_LOGS_EXPORTER=otlp \
    OTEL_METRIC_EXPORT_INTERVAL=15000 \
    OTEL_PYTHON_LOGGING_AUTO_INSTRUMENTATION_ENABLED=true \
    OTEL_PYTHON_DISABLED_INSTRUMENTATIONS=redis \
    OTEL_SEMCONV_STABILITY_OPT_IN=http
EXPOSE 9999
ENTRYPOINT ["opentelemetry-instrument", "python", "app.py"]
