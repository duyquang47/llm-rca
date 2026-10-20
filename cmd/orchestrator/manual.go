// Manual investigation flow: an SRE looks at an incident, decides
// which scan groups to run, then explicitly requests analysis —
// broken into separate HTTP calls instead of the fully-automatic
// analyze() tail in main.go. Route /webhook and handleWebhook/analyze
// are untouched by this file; manual incidents flow through a
// parallel /webhook/manual route that stops at StateReceived instead
// of spawning analyze().
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/duyquang47/llm-rca/internal/builder"
	"github.com/duyquang47/llm-rca/internal/incident"
	"github.com/duyquang47/llm-rca/internal/llm"
	"github.com/duyquang47/llm-rca/internal/prompt"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleWebhookManual parses an Alertmanager webhook exactly like
// handleWebhook and groups the alert into an incident, but never
// starts analysis — the incident sits at StateReceived until an SRE
// drives it through scan/analyze below.
func (s *server) handleWebhookManual(w http.ResponseWriter, r *http.Request) {
	var payload alertmanagerWebhook
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid webhook payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	for _, a := range payload.Alerts {
		if a.Status != "firing" {
			continue
		}
		service := a.Labels["service"]
		if service == "" {
			continue
		}
		alert := incident.Alert{
			Name:     a.Labels["alertname"],
			Service:  service,
			Severity: a.Labels["severity"],
			Message:  a.Annotations["summary"],
			FiredAt:  a.StartsAt,
		}
		inc, isNew := s.grouper.Ingest(alert)
		s.logger.Info("manual alert ingested", "incident_id", inc.ID, "service", service, "is_new_incident", isNew)
	}
	w.WriteHeader(http.StatusAccepted)
}

type incidentView struct {
	ID             string           `json:"id"`
	Services       []string         `json:"services"`
	Alerts         []incident.Alert `json:"alerts"`
	State          string           `json:"state"`
	StartedAt      time.Time        `json:"started_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	ScannedService bool             `json:"scanned_service"`
	ScannedK8s     bool             `json:"scanned_k8s"`
}

func (s *server) viewFor(ctx context.Context, inc *incident.Incident) (incidentView, error) {
	scannedSvc, err := s.manualScans.HasScan(ctx, inc.ID, "service")
	if err != nil {
		return incidentView{}, err
	}
	scannedK8s, err := s.manualScans.HasScan(ctx, inc.ID, "k8s")
	if err != nil {
		return incidentView{}, err
	}
	return incidentView{
		ID:             inc.ID,
		Services:       inc.Services,
		Alerts:         inc.Alerts,
		State:          string(inc.State),
		StartedAt:      inc.StartedAt,
		UpdatedAt:      inc.UpdatedAt,
		ScannedService: scannedSvc,
		ScannedK8s:     scannedK8s,
	}, nil
}

func (s *server) handleListIncidents(w http.ResponseWriter, r *http.Request) {
	incs := s.grouper.List()
	views := make([]incidentView, 0, len(incs))
	for _, inc := range incs {
		v, err := s.viewFor(r.Context(), inc)
		if err != nil {
			s.logger.Error("list incidents: scan status", "incident_id", inc.ID, "err", err)
			http.Error(w, "query scan status: "+err.Error(), http.StatusInternalServerError)
			return
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *server) handleGetIncident(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inc, ok := s.grouper.Get(id)
	if !ok {
		http.Error(w, "incident not found: "+id, http.StatusNotFound)
		return
	}
	v, err := s.viewFor(r.Context(), inc)
	if err != nil {
		s.logger.Error("get incident: scan status", "incident_id", id, "err", err)
		http.Error(w, "query scan status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleScanService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inc, ok := s.grouper.Get(id)
	if !ok {
		http.Error(w, "incident not found: "+id, http.StatusNotFound)
		return
	}
	s.grouper.Touch(id)

	ctx := r.Context()
	scan, err := builder.ScanService(ctx, s.source, windowFor(inc))
	if err != nil {
		s.logger.Error("scan service failed", "incident_id", id, "err", err)
		http.Error(w, "scan service: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := json.Marshal(scan)
	if err != nil {
		http.Error(w, "marshal scan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.manualScans.Save(ctx, id, "service", data); err != nil {
		s.logger.Error("save service scan failed", "incident_id", id, "err", err)
		http.Error(w, "save scan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, scan)
}

func (s *server) handleScanK8s(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inc, ok := s.grouper.Get(id)
	if !ok {
		http.Error(w, "incident not found: "+id, http.StatusNotFound)
		return
	}
	s.grouper.Touch(id)

	ctx := r.Context()
	scan, err := builder.ScanK8s(ctx, s.source, windowFor(inc))
	if err != nil {
		s.logger.Error("scan k8s failed", "incident_id", id, "err", err)
		http.Error(w, "scan k8s: "+err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := json.Marshal(scan)
	if err != nil {
		http.Error(w, "marshal scan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.manualScans.Save(ctx, id, "k8s", data); err != nil {
		s.logger.Error("save k8s scan failed", "incident_id", id, "err", err)
		http.Error(w, "save scan: "+err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, scan)
}

type analyzeResponse struct {
	IncidentID string        `json:"incident_id"`
	Result     llm.RCAResult `json:"result"`
	NotScanned []string      `json:"not_scanned,omitempty"`
	LatencyMs  int64         `json:"latency_ms"`
}

// handleAnalyze assembles a context pack from whatever scan groups
// were saved for this incident (at least one required) and requests
// root-cause analysis from the LLM — the manual-flow counterpart to
// analyze() in main.go, storing to the same incidents table with
// Mode: "manual".
func (s *server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	inc, ok := s.grouper.Get(id)
	if !ok {
		http.Error(w, "incident not found: "+id, http.StatusNotFound)
		return
	}
	s.grouper.Touch(id)
	log := s.logger.With("incident_id", id)

	ctx := r.Context()
	svc, k8s, err := s.manualScans.Latest(ctx, id)
	if err != nil {
		log.Error("load manual scans failed", "err", err)
		http.Error(w, "load scans: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if svc == nil && k8s == nil {
		http.Error(w, "incident "+id+" chua duoc scan - goi scan-service hoac scan-k8s truoc khi analyze", http.StatusBadRequest)
		return
	}

	pack, err := builder.Assemble(windowFor(inc), alertSummaryFor(inc), id, svc, k8s)
	if err != nil {
		log.Error("assemble context pack failed", "err", err)
		inc.State = incident.StateFailed
		http.Error(w, "assemble context pack: "+err.Error(), http.StatusInternalServerError)
		return
	}

	p, err := prompt.Serialize(pack)
	if err != nil {
		log.Error("serialize prompt failed", "err", err)
		inc.State = incident.StateFailed
		http.Error(w, "serialize prompt: "+err.Error(), http.StatusInternalServerError)
		return
	}

	inc.State = incident.StateAnalyzing
	start := time.Now()
	result, usage, err := s.llm.Analyze(ctx, p)
	latency := time.Since(start)
	if err != nil {
		log.Error("llm analyze failed", "err", err)
		inc.State = incident.StateFailed
		http.Error(w, "llm analyze: "+err.Error(), http.StatusInternalServerError)
		return
	}

	packJSON, _ := json.Marshal(pack)
	resultJSON, _ := json.Marshal(result)
	if err := s.store.Insert(ctx, incident.Record{
		IncidentID:    inc.ID,
		ScenarioName:  inc.Services[0],
		StartedAtUnix: inc.StartedAt.Unix(),
		ContextPack:   string(packJSON),
		PackVersion:   pack.Version,
		PromptVersion: "v1",
		LLMModel:      s.llm.Model(),
		LLMOutput:     string(resultJSON),
		TokenCount:    uint32(usage.InputTokens + usage.OutputTokens),
		LatencyMs:     uint32(latency.Milliseconds()),
		Mode:          "manual",
	}); err != nil {
		log.Error("store incident failed", "err", err)
		inc.State = incident.StateFailed
		http.Error(w, "store incident: "+err.Error(), http.StatusInternalServerError)
		return
	}

	inc.State = incident.StateDone
	log.Info("manual analyze done", "top_cause", topCause(result), "latency_ms", latency.Milliseconds())

	writeJSON(w, http.StatusOK, analyzeResponse{
		IncidentID: id,
		Result:     result,
		NotScanned: pack.NotScanned,
		LatencyMs:  latency.Milliseconds(),
	})
}
