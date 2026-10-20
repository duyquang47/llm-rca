package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/duyquang47/llm-rca/internal/jobrunner"
	"github.com/duyquang47/llm-rca/internal/scenario"
)

type jobFlags struct {
	scenarioPath string

	imageTag      string
	namespace     string
	demoNamespace string
	kubeconfig    string
	detach        bool

	chAddr, chUser, chPassword, chDatabase string
}

func parseJobFlags(name string, args []string) (jobFlags, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var f jobFlags
	fs.StringVar(&f.imageTag, "image-tag", "v1.0", "image tag for faultctl/evaluator")
	fs.StringVar(&f.namespace, "namespace", "fault-manager", "namespace to run the Job in")
	fs.StringVar(&f.demoNamespace, "demo-namespace", "demo", "namespace the demo services run in")
	fs.StringVar(&f.kubeconfig, "kubeconfig", "", "path to kubeconfig (default: same resolution as kubectl)")
	fs.BoolVar(&f.detach, "detach", false, "suppress log streaming; still waits for completion")
	fs.BoolVar(&f.detach, "d", false, "shorthand for --detach")
	fs.StringVar(&f.chAddr, "clickhouse-addr", envOr("CLICKHOUSE_ADDR", "clickhouse.observability:9000"), "")
	fs.StringVar(&f.chUser, "clickhouse-user", envOr("CLICKHOUSE_USER", "otel"), "")
	fs.StringVar(&f.chPassword, "clickhouse-password", envOr("CLICKHOUSE_PASSWORD", "otel"), "")
	fs.StringVar(&f.chDatabase, "clickhouse-database", envOr("CLICKHOUSE_DATABASE", "otel"), "")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() < 1 {
		return f, fmt.Errorf("%s requires <scenario-file>", name)
	}
	f.scenarioPath = fs.Arg(0)
	return f, nil
}

func (f jobFlags) renderOpts() jobrunner.Options {
	return jobrunner.Options{
		ImageTag:           f.imageTag,
		Namespace:          f.namespace,
		DemoNamespace:      f.demoNamespace,
		ClickHouseAddr:     f.chAddr,
		ClickHouseUser:     f.chUser,
		ClickHousePassword: f.chPassword,
		ClickHouseDatabase: f.chDatabase,
	}
}

func runJobCmd(name string, args []string) error {
	f, err := parseJobFlags(name, args)
	if err != nil {
		return err
	}

	var kind jobrunner.Kind
	if name == "validate" {
		kind = jobrunner.KindValidate
	} else {
		kind = jobrunner.KindInject
	}

	clientset, err := jobrunner.NewClientset(f.kubeconfig)
	if err != nil {
		return err
	}
	cm, job, err := jobrunner.Render(f.scenarioPath, kind, f.renderOpts())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return jobrunner.Run(ctx, clientset, cm, job, f.detach)
}

func runFullRun(args []string) error {
	f, err := parseJobFlags("full-run", args)
	if err != nil {
		return err
	}

	sc, err := scenario.Load(f.scenarioPath)
	if err != nil {
		return err
	}

	clientset, err := jobrunner.NewClientset(f.kubeconfig)
	if err != nil {
		return err
	}

	fmt.Printf("[rcactl] full-run %s: inject\n", sc.Name)
	injectStart := time.Now()
	cm, job, err := jobrunner.Render(f.scenarioPath, jobrunner.KindInject, f.renderOpts())
	if err != nil {
		return err
	}
	injectCtx, injectCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer injectCancel()
	if err := jobrunner.Run(injectCtx, clientset, cm, job, f.detach); err != nil {
		return fmt.Errorf("inject failed: %w", err)
	}

	fmt.Printf("[rcactl] full-run %s: cho orchestrator tu dong tao incident...\n", sc.Name)
	conn, err := ch.Open(&ch.Options{
		Addr: []string{f.chAddr},
		Auth: ch.Auth{Database: f.chDatabase, Username: f.chUser, Password: f.chPassword},
	})
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}
	const waitTimeout = 3 * time.Minute
	waitCtx, waitCancel := context.WithTimeout(context.Background(), waitTimeout)
	defer waitCancel()
	if err := waitForIncident(waitCtx, conn, injectStart, waitTimeout); err != nil {
		return err
	}

	fmt.Printf("[rcactl] full-run %s: validate\n", sc.Name)
	vcm, vjob, err := jobrunner.Render(f.scenarioPath, jobrunner.KindValidate, f.renderOpts())
	if err != nil {
		return err
	}
	validateCtx, validateCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer validateCancel()
	if err := jobrunner.Run(validateCtx, clientset, vcm, vjob, f.detach); err != nil {
		return fmt.Errorf("validate failed: %w", err)
	}

	fmt.Printf("[rcactl] full-run %s: hoan tat\n", sc.Name)
	return nil
}

// waitForIncident polls ClickHouse for an incident whose started_at
// falls in the same window evaluator.findIncident later searches
// (started_at in [since-30s, now]) — see cmd/evaluator/main.go's
// findIncident. Polling instead of a fixed sleep because LLM latency
// varies (up to the 180s client timeout in internal/llm/gemini).
func waitForIncident(ctx context.Context, conn ch.Conn, since time.Time, timeout time.Duration) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		rows, err := conn.Query(ctx, `
			SELECT incident_id FROM incidents
			WHERE started_at >= ? AND started_at <= ?
			ORDER BY started_at ASC LIMIT 1`, since.Add(-30*time.Second), time.Now())
		if err != nil {
			return fmt.Errorf("query incidents: %w", err)
		}
		found := rows.Next()
		rows.Close()
		if found {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("orchestrator chua tao incident sau %s - kiem tra alert co ban khong", timeout)
		case <-ticker.C:
		}
	}
}
