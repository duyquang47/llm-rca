// Package otelinit centralizes OpenTelemetry SDK setup (traces, logs,
// metrics) shared by every service-chain binary, and provides an
// slog.Logger bridged to the log SDK so every log line carries trace_id
// and span_id automatically. See CLAUDE.md §4.3.
package otelinit

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config controls where telemetry is exported and how the service
// identifies itself. Endpoint is host:port with no scheme (grpc target).
type Config struct {
	ServiceName string
	Endpoint    string // e.g. "otel-collector.observability:4317"
}

// Shutdown flushes and closes all telemetry providers. Call it via defer
// right after Setup succeeds.
type Shutdown func(context.Context) error

// Logger is an slog.Logger whose handler emits through the OTel log SDK.
// Every call using the *Context variants (InfoContext, ErrorContext, ...)
// automatically picks up trace_id/span_id from the active span in ctx —
// this is the mechanism CLAUDE.md §4.3 requires; slog.Info without
// context does NOT get trace correlation.
type Logger = *slog.Logger

// Setup wires up trace/log/metric providers as globals (via
// otel.SetTracerProvider etc.) and returns a Logger plus a Shutdown func.
func Setup(ctx context.Context, cfg Config) (Logger, Shutdown, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build resource: %w", err)
	}

	traceExp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build trace exporter: %w", err)
	}
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tracerProvider)

	// The global default propagator is a no-op composite — without this,
	// neither otelhttp's client/server headers nor our stream
	// traceparent field carry trace context, and every downstream hop
	// starts a disconnected trace. See CLAUDE.md §4.3.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logExp, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(cfg.Endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build log exporter: %w", err)
	}
	loggerProvider := log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(logExp)),
		log.WithResource(res),
	)

	metricExp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("build metric exporter: %w", err)
	}
	meterProvider := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(metricExp, metric.WithInterval(15*time.Second))),
		metric.WithResource(res),
	)
	otel.SetMeterProvider(meterProvider)

	// Mirrors every log record onto the active span as a span event, so
	// Jaeger's per-span "Logs" tab (already part of its UI, no Jaeger-side
	// config needed) shows the same log lines ClickHouse gets — without
	// this, Jaeger has no way to show log data at all, since it has no
	// log-search UI of its own and only renders logs that arrive as span
	// events. Wraps rather than replaces the otelslog handler so the
	// existing ClickHouse-bound log pipeline (otel-collector.yaml's logs
	// pipeline) is completely unaffected.
	logger := slog.New(newSpanEventHandler(otelslog.NewHandler(cfg.ServiceName, otelslog.WithLoggerProvider(loggerProvider))))

	shutdown := func(ctx context.Context) error {
		var errs []error
		if err := tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := loggerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return fmt.Errorf("shutdown errors: %v", errs)
		}
		return nil
	}

	return logger, shutdown, nil
}
