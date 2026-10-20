// Command rcactl is the unified operator CLI for llm-rca: it drives the
// 13-scenario fault-injection benchmark (inject/validate/full-run) and
// gives an SRE a manual, step-by-step investigation flow
// (list/show/scan/analyze) as an alternative to the automatic
// alert->context-pack->LLM pipeline. See the plan this was built from
// for the full design.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "inject":
		err = runJobCmd("inject", args)
	case "validate":
		err = runJobCmd("validate", args)
	case "full-run":
		err = runFullRun(args)
	case "list":
		err = runList(args)
	case "show":
		err = runShow(args)
	case "scan":
		err = runScan(args)
	case "analyze":
		err = runAnalyze(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "rcactl: unknown subcommand %q\n\n", cmd)
		usage()
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rcactl - van hanh benchmark 13 scenario + dieu tra thu cong cho llm-rca

Job orchestration (dung client-go + kubeconfig cuc bo, khong can orchestrator):
  rcactl inject   <scenario-file> [flags]
  rcactl validate <scenario-file> [flags]
  rcactl full-run <scenario-file> [flags]

Flags cho inject/validate/full-run:
  -d, --detach              khong stream log, chi cho job xong roi bao ket qua
  --image-tag string        tag image faultctl/evaluator (mac dinh "v1.0")
  --namespace string        namespace chay Job (mac dinh "fault-manager")
  --demo-namespace string   namespace cua demo services (mac dinh "demo")
  --kubeconfig string       duong dan kubeconfig (mac dinh nhu kubectl)
  --clickhouse-addr/user/password/database string

Dieu tra thu cong (goi HTTP API tren orchestrator qua bien moi truong
ORCHESTRATOR_URL, mac dinh http://localhost:8080 - can chay truoc:
  kubectl port-forward svc/orchestrator -n observability 8080:8080
):
  rcactl list
  rcactl show     <incident-id>
  rcactl scan     <incident-id> service|k8s
  rcactl analyze  <incident-id>
`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
