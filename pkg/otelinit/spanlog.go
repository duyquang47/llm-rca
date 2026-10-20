package otelinit

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// spanEventHandler wraps another slog.Handler (the otelslog bridge, in
// practice) and additionally records every log record as an event on the
// span active in the record's context, if any. See the comment at its
// construction site in Setup for why this exists.
type spanEventHandler struct {
	next slog.Handler
}

func newSpanEventHandler(next slog.Handler) *spanEventHandler {
	return &spanEventHandler{next: next}
}

func (h *spanEventHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *spanEventHandler) Handle(ctx context.Context, record slog.Record) error {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		attrs := make([]attribute.KeyValue, 0, record.NumAttrs()+1)
		attrs = append(attrs, attribute.String("log.severity", record.Level.String()))
		record.Attrs(func(a slog.Attr) bool {
			attrs = append(attrs, attribute.String(a.Key, a.Value.String()))
			return true
		})
		span.AddEvent(record.Message, trace.WithAttributes(attrs...))
	}
	return h.next.Handle(ctx, record)
}

func (h *spanEventHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &spanEventHandler{next: h.next.WithAttrs(attrs)}
}

func (h *spanEventHandler) WithGroup(name string) slog.Handler {
	return &spanEventHandler{next: h.next.WithGroup(name)}
}
