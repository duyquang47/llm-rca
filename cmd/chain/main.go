// Command chain is the single binary deployed as all six service-chain
// pods (gateway, order, inventory, payment, thirdparty-mock,
// notify-worker); behavior is entirely determined by CONFIG_PATH.
// See CLAUDE.md §4.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/duyquang47/llm-rca/internal/chain/config"
	"github.com/duyquang47/llm-rca/internal/chain/faults"
	"github.com/duyquang47/llm-rca/internal/chain/handler"
	"github.com/duyquang47/llm-rca/pkg/otelinit"
)

// internalFaultAddr must only be reachable inside the cluster network,
// never through a public k8s Service (CLAUDE.md §4.4).
const internalFaultAddr = ":9999"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		return fmt.Errorf("CONFIG_PATH env var is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = cfg.ServiceName
	}
	endpoint := strings.TrimPrefix(strings.TrimPrefix(
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "https://"), "http://")
	if endpoint == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT env var is required")
	}

	ctx := context.Background()
	logger, shutdown, err := otelinit.Setup(ctx, otelinit.Config{
		ServiceName: serviceName,
		Endpoint:    endpoint,
	})
	if err != nil {
		return fmt.Errorf("otel setup: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(shutdownCtx)
	}()

	faultCtl := faults.NewController()
	srv := handler.NewServer(cfg, faultCtl, logger)

	publicServer := &http.Server{Addr: cfg.Listen, Handler: srv.Mux()}

	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/_fault", faultCtl.Handler())
	internalServer := &http.Server{Addr: internalFaultAddr, Handler: internalMux}

	go func() {
		logger.Info("public server listening", "addr", cfg.Listen, "service", serviceName)
		if err := publicServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("public server error", "err", err)
		}
	}()
	go func() {
		logger.Info("internal fault control listening", "addr", internalFaultAddr)
		if err := internalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("internal fault server error", "err", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = publicServer.Shutdown(shutdownCtx)
	_ = internalServer.Shutdown(shutdownCtx)

	return nil
}
