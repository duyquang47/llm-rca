package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// orchestratorURL requires `kubectl port-forward svc/orchestrator -n
// observability 8080:8080` to already be running — the Service has no
// Ingress/NodePort (deploy/rca/orchestrator.yaml), by design, since the
// cluster's nodes have no direct route from outside.
func orchestratorURL() string {
	return envOr("ORCHESTRATOR_URL", "http://localhost:8080")
}

func getJSON(path string) error {
	resp, err := http.Get(orchestratorURL() + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	return printResponse(resp)
}

func postJSON(path string, body []byte) error {
	resp, err := http.Post(orchestratorURL()+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	return printResponse(resp)
}

func printResponse(resp *http.Response) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("orchestrator tra ve %s: %s", resp.Status, string(raw))
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		// Not JSON — print raw rather than fail the command over formatting.
		fmt.Println(string(raw))
		return nil
	}
	fmt.Println(pretty.String())
	return nil
}

func runList(args []string) error {
	return getJSON("/incidents")
}

func runShow(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("show requires <incident-id>")
	}
	return getJSON("/incidents/" + args[0])
}

func runScan(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("scan requires <incident-id> service|k8s")
	}
	id, scanType := args[0], args[1]
	var path string
	switch scanType {
	case "service":
		path = "/incidents/" + id + "/scan-service"
	case "k8s":
		path = "/incidents/" + id + "/scan-k8s"
	default:
		return fmt.Errorf("loai scan khong hop le %q - phai la \"service\" hoac \"k8s\"", scanType)
	}
	return postJSON(path, nil)
}

func runAnalyze(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("analyze requires <incident-id>")
	}
	return postJSON("/incidents/"+args[0]+"/analyze", nil)
}
