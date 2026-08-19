// Package handler implements the generic, config-driven route handler
// shared by all six service-chain deployments. See CLAUDE.md §4.2.
package handler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/duyquang47/llm-rca/internal/chain/config"
	"github.com/duyquang47/llm-rca/internal/chain/faults"
)

type Server struct {
	cfg    config.Config
	faults *faults.Controller
	log    *slog.Logger
	client *http.Client

	mu       sync.Mutex
	redisCli map[string]*redis.Client
	dbPools  map[string]*pgxpool.Pool // keyed by dsn; recreated if pool_size changes
	dbSizes  map[string]int
}

func NewServer(cfg config.Config, faultCtl *faults.Controller, logger *slog.Logger) *Server {
	return &Server{
		cfg:      cfg,
		faults:   faultCtl,
		log:      logger,
		client:   &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
		redisCli: make(map[string]*redis.Client),
		dbPools:  make(map[string]*pgxpool.Pool),
		dbSizes:  make(map[string]int),
	}
}

// Mux builds the HTTP handler for all configured routes, wrapped in
// otelhttp so every inbound request reads traceparent and starts a
// server span (CLAUDE.md §4.3).
func (s *Server) Mux() http.Handler {
	mux := http.NewServeMux()
	for _, route := range s.cfg.Routes {
		mux.HandleFunc(route.Path, s.handle(route))
	}
	return otelhttp.NewHandler(mux, "server")
}

func (s *Server) handle(route config.Route) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if err := s.faults.Apply(ctx, route.Path); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		if route.Cache != nil {
			if err := s.cacheGet(ctx, r, route.Cache); err != nil {
				s.log.WarnContext(ctx, "cache get failed", "err", err)
			}
		}

		if route.DB != nil {
			if err := s.query(ctx, r, route.DB); err != nil {
				s.log.ErrorContext(ctx, "db query failed", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}

		for _, call := range route.Calls {
			if err := s.callDownstream(ctx, r, call); err != nil {
				s.log.ErrorContext(ctx, "downstream call failed", "target", call.Name, "err", err)
				http.Error(w, "downstream failed", http.StatusBadGateway)
				return
			}
		}

		if route.Publish != nil {
			if err := s.publish(ctx, route.Publish); err != nil {
				s.log.WarnContext(ctx, "publish failed", "err", err)
			}
		}

		w.WriteHeader(http.StatusOK)
	}
}

func (s *Server) redisClient(addr string) *redis.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cli, ok := s.redisCli[addr]; ok {
		return cli
	}
	cli := redis.NewClient(&redis.Options{Addr: addr})
	s.redisCli[addr] = cli
	return cli
}

func (s *Server) cacheGet(ctx context.Context, r *http.Request, c *config.Cache) error {
	key := substituteParams(c.Key, r)
	return s.redisClient(c.Addr).Get(ctx, key).Err()
}

// dbPool returns a connection pool sized by the fault-effective pool_size
// (CLAUDE.md §4.4 precedence note), recreating it if the effective size
// has changed since the last call — this is what makes pool exhaustion
// (case #3) toggleable at runtime via /_fault.
func (s *Server) dbPool(ctx context.Context, db *config.DB) (*pgxpool.Pool, error) {
	effectiveSize := s.faults.EffectivePoolSize(routeKeyForDB(db), db.PoolSize)
	if effectiveSize <= 0 {
		effectiveSize = 5
	}

	s.mu.Lock()
	pool, ok := s.dbPools[db.DSN]
	sameSize := ok && s.dbSizes[db.DSN] == effectiveSize
	s.mu.Unlock()
	if ok && sameSize {
		return pool, nil
	}

	poolCfg, err := pgxpool.ParseConfig(db.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	poolCfg.MaxConns = int32(effectiveSize)

	newPool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	s.mu.Lock()
	if old, ok := s.dbPools[db.DSN]; ok {
		old.Close()
	}
	s.dbPools[db.DSN] = newPool
	s.dbSizes[db.DSN] = effectiveSize
	s.mu.Unlock()

	return newPool, nil
}

// routeKeyForDB is a placeholder key space for pool-size faults until the
// fault controller addresses faults per-service rather than per-route;
// DB faults are set with route="db".
func routeKeyForDB(_ *config.DB) string { return "db" }

func (s *Server) query(ctx context.Context, r *http.Request, db *config.DB) error {
	pool, err := s.dbPool(ctx, db)
	if err != nil {
		return err
	}

	id := r.URL.Query().Get("id")
	if id == "" {
		id = "1"
	}

	run := func() error {
		rows, err := pool.Query(ctx, db.Query, id)
		if err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	}

	if err := run(); err != nil {
		return fmt.Errorf("query: %w", err)
	}

	if db.NPlusOne {
		// Simulate the N+1 pattern: re-issue the same query per "row" instead
		// of batching, so span count balloons — the signal case #4 relies on.
		const simulatedRows = 5
		for i := 0; i < simulatedRows; i++ {
			if err := run(); err != nil {
				return fmt.Errorf("n+1 query %d: %w", i, err)
			}
		}
	}

	return nil
}

func (s *Server) callDownstream(ctx context.Context, inbound *http.Request, call config.Call) error {
	var lastErr error
	attempts := call.Retry + 1

	for attempt := 0; attempt < attempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, call.Timeout)
		req, err := http.NewRequestWithContext(callCtx, inbound.Method, call.URL, nil)
		if err != nil {
			cancel()
			return fmt.Errorf("build request to %s: %w", call.Name, err)
		}

		resp, err := s.client.Do(req)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("%s returned %d", call.Name, resp.StatusCode)
			continue
		}
		return nil
	}

	return fmt.Errorf("all %d attempts to %s failed: %w", attempts, call.Name, lastErr)
}

func (s *Server) publish(ctx context.Context, pub *config.Publish) error {
	if pub.Addr == "" {
		return fmt.Errorf("publish.addr is required in config")
	}

	// Propagate trace context through the stream so notify-worker's
	// consumer span links back to the request that published it.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	values := map[string]any{"published_at": time.Now().UTC().Format(time.RFC3339)}
	if tp, ok := carrier["traceparent"]; ok {
		values["traceparent"] = tp
	}

	return s.redisClient(pub.Addr).XAdd(ctx, &redis.XAddArgs{
		Stream: pub.Stream,
		Values: values,
	}).Err()
}

// substituteParams replaces {name} placeholders in a template with query
// params from the request, e.g. "user:{user_id}" + ?user_id=42 -> "user:42".
func substituteParams(template string, r *http.Request) string {
	out := template
	for key, vals := range r.URL.Query() {
		if len(vals) == 0 {
			continue
		}
		out = strings.ReplaceAll(out, "{"+key+"}", vals[0])
	}
	return out
}
