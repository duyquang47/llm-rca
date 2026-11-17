// Package consumer implements notify-worker's Redis Stream consumer
// loop — the one deployment in the chain driven by messages instead of
// inbound HTTP requests. See CLAUDE.md §2.1, §7 (case #9 consumer lag,
// case #10 poison message).
package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/duyquang47/llm-rca/internal/chain/config"
)

// Run blocks, consuming cfg.Stream via a consumer group, until ctx is
// canceled.
//
// Deliberate design choice: a message that fails to carry the expected
// "published_at" field is NOT recovered from — processEntry panics. This
// simulates the "poison message kills the consumer" failure mode
// (case #10). Swallowing the error here would make that scenario
// untestable. Consumer-lag backlog (case #9) needs no special handling:
// stopping the consumer (scale to 0) simply leaves entries pending.
func Run(ctx context.Context, cfg config.Consume, logger *slog.Logger) error {
	cli := redis.NewClient(&redis.Options{Addr: cfg.Addr})
	defer cli.Close()

	// Group creation is retried rather than fatal: a single failure here
	// (redis OOM-restarting, not up yet) used to return from Run and leave
	// the pod Running with no consumer at all.
	if !ensureGroup(ctx, cli, cfg, logger) {
		return nil
	}

	logger.InfoContext(ctx, "notify-worker consuming", "stream", cfg.Stream, "group", cfg.Group)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		streams, err := cli.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    cfg.Group,
			Consumer: cfg.Consumer,
			Streams:  []string{cfg.Stream, ">"},
			Count:    10,
			Block:    5 * time.Second,
		}).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			logger.ErrorContext(ctx, "xreadgroup failed", "err", err)
			// redis has no volume here, so a restart drops the group
			// along with the stream — recreate it instead of
			// failing every read forever.
			if isNoGroup(err) {
				if !ensureGroup(ctx, cli, cfg, logger) {
					return nil
				}
				continue
			}
			if !sleepCtx(ctx, time.Second) {
				return nil
			}
			continue
		}

		for _, s := range streams {
			for _, entry := range s.Messages {
				processEntry(ctx, cli, cfg, entry, logger)
			}
		}
	}
}

func processEntry(ctx context.Context, cli *redis.Client, cfg config.Consume, entry redis.XMessage, logger *slog.Logger) {
	msgCtx := ctx
	if tp, ok := entry.Values["traceparent"].(string); ok && tp != "" {
		msgCtx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": tp})
	}
	msgCtx, span := otel.Tracer(cfg.Consumer).Start(msgCtx, "notify-worker process")
	defer span.End()

	publishedAt, _ := entry.Values["published_at"].(string)
	if publishedAt == "" {
		panic(fmt.Sprintf("poison message %s on stream %s: missing published_at field", entry.ID, cfg.Stream))
	}

	logger.InfoContext(msgCtx, "processed notification", "entry_id", entry.ID, "published_at", publishedAt)
	if err := cli.XAck(msgCtx, cfg.Stream, cfg.Group, entry.ID).Err(); err != nil {
		logger.ErrorContext(msgCtx, "xack failed", "entry_id", entry.ID, "err", err)
	}
}

// ensureGroup creates the consumer group (and stream), retrying with
// backoff until it succeeds or ctx is canceled (returns false).
func ensureGroup(ctx context.Context, cli *redis.Client, cfg config.Consume, logger *slog.Logger) bool {
	backoff := time.Second
	for {
		err := cli.XGroupCreateMkStream(ctx, cfg.Stream, cfg.Group, "0").Err()
		if err == nil || isBusyGroup(err) {
			return true
		}
		logger.ErrorContext(ctx, "create consumer group failed, retrying", "err", err, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return false
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func isNoGroup(err error) bool {
	return strings.HasPrefix(err.Error(), "NOGROUP")
}

func isBusyGroup(err error) bool {
	return strings.Contains(err.Error(), "BUSYGROUP")
}
