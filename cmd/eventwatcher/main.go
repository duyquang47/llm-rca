// Command eventwatcher replaces kubernetes-event-exporter, whose v1.7.0
// binary silently stops processing events after the first
// "involvedObject not found" lookup error against this k3s v1.36
// cluster — reproduced identically across two image sources/registries
// and several config changes, so it's a binary/API-version
// incompatibility, not something fixable via config. CLAUDE.md §3
// already commits to k8s.io/client-go for the RCA services, so this
// uses the same library via a SharedInformer (handles reconnect/resync
// itself) instead of depending on a third-party tool.
//
// Writes one JSON object per line to a file that otel-collector's
// filelog receiver tails — see deploy/observability/otel-collector.yaml.
//
// Scoped to a single namespace (WATCH_NAMESPACE, default "demo") —
// CLAUDE.md §2.4 requires the chaos namespace excluded from every log
// pipeline, and watching cluster-wide leaked exactly that: running
// scenario #1 for real showed the LLM citing a k8s Event about
// fault-controller's own pod ("faultctl-timeout-cascade-...' chạy
// trong namespace 'chaos'") as root-cause evidence. Filtering only at
// the ClickHouse query layer wouldn't be enough — the leaking data
// would still be sitting in otel_logs for anything else that queries
// it, so this is fixed at the collection source instead.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type eventRecord struct {
	Timestamp          string `json:"timestamp"`
	Namespace          string `json:"namespace"`
	InvolvedObjectKind string `json:"involved_object_kind"`
	InvolvedObjectName string `json:"involved_object_name"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	Type               string `json:"type"`
	Count              int32  `json:"count"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	outPath := os.Getenv("EVENTS_LOG_PATH")
	if outPath == "" {
		outPath = "/var/log/k8s-events/events.log"
	}
	watchNamespace := os.Getenv("WATCH_NAMESPACE")
	if watchNamespace == "" {
		watchNamespace = "demo"
	}

	f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open output file: %w", err)
	}
	defer f.Close()

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}

	write := func(obj any) {
		event, ok := obj.(*corev1.Event)
		if !ok {
			return
		}
		record := eventRecord{
			Timestamp:          eventTimestamp(event).Format(time.RFC3339Nano),
			Namespace:          event.Namespace,
			InvolvedObjectKind: event.InvolvedObject.Kind,
			InvolvedObjectName: event.InvolvedObject.Name,
			Reason:             event.Reason,
			Message:            event.Message,
			Type:               event.Type,
			Count:              event.Count,
		}
		line, err := json.Marshal(record)
		if err != nil {
			slog.Error("marshal event", "err", err)
			return
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			slog.Error("write event", "err", err)
		}
	}

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithNamespace(watchNamespace))
	informer := factory.Core().V1().Events().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: write,
		// Kubernetes coalesces repeated identical events into Count
		// increments on the same object instead of creating new ones —
		// without this, a burst of the same event (e.g. repeated
		// BackOff) would only ever be captured once.
		UpdateFunc: func(_, newObj any) { write(newObj) },
	})

	stop := make(chan struct{})
	slog.Info("eventwatcher starting", "output", outPath, "namespace", watchNamespace)
	factory.Start(stop)
	factory.WaitForCacheSync(stop)

	select {} // run forever; the informer's own goroutines do the work
}

func eventTimestamp(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
