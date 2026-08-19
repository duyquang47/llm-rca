# LLM-based RCA for Cloud/Microservices — Project Context

Tài liệu này là context gốc cho toàn bộ dự án. Đọc kỹ trước khi sinh code.

---

## 1. Mục tiêu

Xây dựng hệ thống dùng LLM để tóm tắt log/trace/alert và gợi ý nguyên nhân gốc (root cause)
cho các dịch vụ chạy trên môi trường Kubernetes.

Đây là **POC nghiên cứu có đo đạc**, không phải sản phẩm. Tiêu chí thành công là:
top-1 accuracy và MRR trên bộ 13 kịch bản lỗi có ground truth, không phải "câu trả lời trông hợp lý".

Ràng buộc phần cứng: 1 máy, 8–12 CPU / 16GB RAM, chạy k3s single-node.
Ngân sách tài nguyên mục tiêu: **≈ 4.5 CPU, ≈ 4.6Gi RAM** ở trạng thái nền.

### Nguyên tắc thiết kế xuyên suốt

1. **Giá trị demo đến từ độ đa dạng failure mode, không phải số lượng service.**
   Ít service, nhưng mỗi service inject được nhiều loại lỗi.
2. **Một kho dữ liệu duy nhất (ClickHouse) cho trace + log.** Lý do không phải RAM mà là code:
   context-builder chỉ viết SQL thay vì 3 adapter + join thủ công.
3. **Một image duy nhất cho toàn bộ service chain.** Topology định nghĩa bằng config, không phải code.
4. **Fault controller phải cách ly tuyệt đối khỏi telemetry.** Nếu rò rỉ, LLM đọc được đáp án và
   toàn bộ kết quả đánh giá trở nên vô nghĩa.
5. **Đi theo chiều dọc, không theo chiều ngang.** Làm 1 kịch bản end-to-end trước khi tổng quát hóa.
6. **Tách bạch hai giai đoạn, ranh giới là file JSON `ContextPack`.** Giai đoạn 1 tất định
   (test được bằng assertion), giai đoạn 2 ngẫu nhiên (chỉ đo được bằng thống kê). Nhờ đó
   khi kết quả sai, biết ngay lỗi thuộc giai đoạn nào — xem mục 10.

---

## 2. Kiến trúc tổng thể

Bốn namespace, ~21 pod.

### 2.1. Namespace `demo` — workload mô phỏng

```
k6 → gateway → order ──┬──→ inventory ──→ postgres
                       ├──→ payment ────→ thirdparty-mock
                       ├──→ redis (cache)
                       └──→ redis stream ──→ notify-worker
```

Sáu pod (`gateway`, `order`, `inventory`, `payment`, `thirdparty-mock`, `notify-worker`)
chạy **cùng một image Go**, khác nhau duy nhất ở ConfigMap được mount.

Chuỗi gọi sâu 4 tầng (`gateway → order → inventory → postgres`) là bắt buộc — cần thiết để
chứng minh trường hợp root cause cách symptom 2 hop.

### 2.2. Namespace `observability` — thu thập và lưu trữ

```
service pod (OTel SDK)
   │ push OTLP gRPC :4317
   ▼
otel-collector  ──[traces, logs]──→ ClickHouse
       │        ──[metrics]──────→ :8889 ←─ scrape ─ Prometheus → Alertmanager
       └────────[traces]─────────→ Jaeger+badger (tùy chọn, chỉ để xem waterfall)

kube-state-metrics ──┐
event-exporter ──────┴→ (metrics/logs qua collector)
Grafana → đọc ClickHouse + Prometheus
```

**Trace và log dùng mô hình push. Metric dùng mô hình pull.** Đây là chỗ hay gây nhầm.

### 2.3. Namespace `rca` — pipeline phân tích

```
Alertmanager webhook
   ▼
incident-orchestrator   (dedupe, gom nhóm, state machine, retry)
   ▼
context-builder         (query ClickHouse → nén → context pack)
   ▼
LLM API                 (trả JSON có thứ hạng nguyên nhân)
   ▼
bảng `incidents` trong ClickHouse
```

Ghi chú kiến trúc: vì dùng kho dữ liệu hợp nhất, **context-collector là module bên trong
context-builder**, không tách thành process riêng. Nếu đề tài yêu cầu thể hiện tính module,
vẫn giữ ranh giới interface rõ ràng ở tầng package (`internal/collector`, `internal/builder`).

### 2.4. Namespace `chaos` — điều khiển và đánh giá

```
fault-controller  → đọc scenario YAML → gọi /_fault hoặc patch k8s resource
                  → ghi ground truth vào bảng riêng
evaluator         → đối chiếu LLM output với ground truth → top-1 accuracy, MRR
k6                → sinh tải nền 20–50 rps
```

**Ràng buộc cách ly (bắt buộc, không thương lượng):**
- Namespace `chaos` bị exclude khỏi mọi scrape config và log pipeline.
- Fault middleware trong service chain **không được log/span gì về việc inject**.
  Một request bị inject latency phải trông y hệt một request chậm thật.
- NetworkPolicy chặn context-builder truy cập ground truth store.

> **Ghi chú bổ sung — đường rò rỉ qua k8s Event:** với các fault cần patch trực tiếp k8s resource
> thay vì qua `/_fault` (VD case #6, hạ CPU limit), k8s Event sinh ra (bị `kubernetes-event-exporter`
> bắt theo mục 5) mang namespace `demo`, **không** bị chặn bởi luật loại-trừ namespace `chaos`, và
> không chứa từ khóa nào bị chặn ở `must_not_include` (mục 7, mục 10 GĐ1). Bài test string-match
> hiện tại cho cảm giác an toàn giả ở đường rò rỉ này. Cần một test riêng xác nhận k8s Event loại
> "patch trực tiếp" không tiết lộ nguồn gốc chaos (VD kiểm tra `reason`/`message` không chứa gợi ý
> can thiệp thủ công bất thường), trước khi coi ranh giới cách ly là đã được kiểm chứng đầy đủ.

---

## 3. Tech stack

| Lớp | Lựa chọn | Ghi chú |
|---|---|---|
| Service chain | Go 1.22+, `net/http`, `otelhttp` | 1 binary, config-driven |
| DB | PostgreSQL 16 | |
| Cache + Queue | Redis 7 (Streams) | Thay NATS để giảm 1 component |
| Instrumentation | OpenTelemetry Go SDK | traces + logs + metrics, OTLP gRPC |
| Collector | otel-collector-contrib | cần `k8sattributes` processor |
| Trace + Log store | ClickHouse 24.x single node | TTL 48h |
| Metric store | Prometheus | retention 24h, scrape 15s |
| Alert | Alertmanager | webhook → orchestrator |
| UI | Grafana | plugin ClickHouse datasource |
| Trace UI (tùy chọn) | Jaeger + badger storage | ~200Mi, chỉ để demo |
| RCA services | **Go 1.22+** | thống nhất một ngôn ngữ với service chain |
| ClickHouse client | `github.com/ClickHouse/clickhouse-go/v2` | driver chính thức, hỗ trợ native protocol |
| k8s client | `k8s.io/client-go` | đọc events, deployment history |
| Log clustering | **tự implement Drain** (xem 6.3) | Go không có port drain3 trưởng thành |
| HTTP framework | `net/http` + `chi` (hoặc stdlib thuần) | tránh framework nặng |
| Load gen | k6 | |
| Orchestration | k3s single-node (tắt traefik, servicelb) | |

**LLM chạy qua API** (Claude/GPT/Gemini). Không chạy model local ở đường chạy chính —
CPU-only inference 3–8 tok/s sẽ biến mỗi lần suy luận thành 3–5 phút và làm hỏng vòng lặp thử nghiệm.

---

## 4. Đặc tả service chain (module quan trọng nhất)

### 4.1. Yêu cầu cốt lõi

Một binary, hành vi hoàn toàn do file YAML quyết định. Deploy 6 lần với 6 ConfigMap.

```yaml
# ví dụ: order-config.yaml
service_name: order
listen: ":8080"
routes:
  - path: /create
    cache:
      addr: "redis:6379"
      key: "user:{user_id}"
      ttl: 60s
    calls:
      - {name: inventory, url: "http://inventory:8080/check", timeout: 500ms, retry: 2}
      - {name: payment,   url: "http://payment:8080/charge",  timeout: 3s,    retry: 0}
    publish:
      stream: "notifications"
```

```yaml
# ví dụ: inventory-config.yaml
service_name: inventory
listen: ":8080"
routes:
  - path: /check
    db:
      dsn: "postgres://app:pass@postgres:5432/shop"
      query: "SELECT stock FROM items WHERE id = $1"
      pool_size: 20        # hạ xuống 2 để demo pool exhaustion
      n_plus_one: false    # bật để demo N+1 query
```

### 4.2. Handler generic

```go
func (s *Server) handle(route Route) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()   // đã chứa trace context từ header traceparent

        if err := s.faults.Apply(ctx, route.Path); err != nil {
            http.Error(w, "internal error", 500); return
        }
        if route.Cache != nil { s.cacheGet(ctx, route.Cache) }
        if route.DB != nil    { s.query(ctx, route.DB) }

        for _, call := range route.Calls {
            if err := s.callDownstream(ctx, call); err != nil {
                http.Error(w, "downstream failed", 502); return
            }
        }
        if route.Publish != nil { s.publish(ctx, route.Publish) }
        w.WriteHeader(200)
    }
}
```

### 4.3. Instrumentation — hai dòng quyết định thành bại

```go
// server: đọc traceparent từ request đến, tạo span con
handler := otelhttp.NewHandler(mux, "server")

// client: chèn traceparent vào request đi ra → đây là thứ nối các hop lại
client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}
```

**Bắt buộc:** mọi log line phải mang `trace_id` và `span_id`. Dùng OTel logging bridge với
`log.InfoContext(ctx, ...)`. Không có nó, câu SQL join log↔trace của context-builder trả về rỗng
và toàn bộ pipeline vô dụng.

### 4.4. Fault middleware

Endpoint điều khiển: `POST /_fault` (chỉ nghe trên port nội bộ, không expose qua Service public).

```json
{
  "route": "/check",
  "latency": {"probability": 1.0, "ms": 800},
  "error":   {"probability": 0.0, "code": 500},
  "cpu_burn": false,
  "mem_leak": false,
  "pool_size": 2
}
```

Trạng thái fault lưu in-memory, reset khi pod restart. Fault controller re-apply sau mỗi restart.

**Tuyệt đối không log, không tạo span, không expose metric nào về trạng thái fault.**

> **Ghi chú bổ sung — thứ tự ưu tiên khi trùng tham số:** một số tham số (VD `pool_size`) xuất
> hiện cả ở ConfigMap tĩnh (mục 4.1) lẫn payload `/_fault` động. Quy ước: giá trị từ `/_fault`
> (in-memory) **luôn override** giá trị ConfigMap trong lúc fault đang active; khi fault reset
> (hoặc pod restart mà chưa được re-apply), quay lại giá trị ConfigMap. Handler đọc giá trị hiệu
> lực (VD kích thước connection pool) từ fault state trước, fallback về config nếu không có fault
> nào đang active cho tham số đó.

### 4.5. Deployment pattern

```yaml
spec:
  containers:
    - name: app
      image: chain-service:1.0        # giống hệt ở cả 6 Deployment
      env:
        - {name: CONFIG_PATH, value: /etc/chain/config.yaml}
        - {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: "http://otel-collector.observability:4317"}
        - {name: OTEL_SERVICE_NAME, valueFrom: {configMapKeyRef: {...}}}
      resources:
        limits: {memory: 64Mi, cpu: 200m}   # limit thấp CÓ CHỦ ĐÍCH để demo OOMKill
  volumes:
    - name: cfg
      configMap: {name: order-config}       # chỉ dòng này khác giữa các service
```

---

## 5. Cấu hình otel-collector

```yaml
receivers:
  otlp:
    protocols:
      grpc: {endpoint: 0.0.0.0:4317}

processors:
  k8sattributes:
    extract:
      metadata: [k8s.pod.name, k8s.deployment.name, k8s.node.name, k8s.namespace.name]
  batch: {timeout: 5s}

exporters:
  clickhouse:
    endpoint: tcp://clickhouse.observability:9000
    database: otel
    ttl: 48h
  prometheus:
    endpoint: 0.0.0.0:8889

service:
  pipelines:
    traces:  {receivers: [otlp], processors: [k8sattributes, batch], exporters: [clickhouse]}
    logs:    {receivers: [otlp], processors: [k8sattributes, batch], exporters: [clickhouse]}
    metrics: {receivers: [otlp], processors: [k8sattributes, batch], exporters: [prometheus]}
```

Exporter ClickHouse tự tạo bảng `otel_traces`, `otel_logs` theo schema chuẩn — không tự thiết kế schema.

**Sampling: 100%.** Tải chỉ 20–50 rps nên không cần tail sampling. Ở 30 rps, một window 5 phút
có ~9000 request, thừa để p99 có ý nghĩa thống kê.

**Bắt buộc cài thêm:** `kube-state-metrics` (pod restart count, resource limit vs request,
container status) và `kubernetes-event-exporter` (OOMKilled, BackOff, FailedScheduling).
Thiếu hai thứ này thì phần "context về hệ thống" hoàn toàn rỗng — k8s event mặc định chỉ
sống 1 giờ trong etcd rồi biến mất.

### 5.1. Bốn khoảng trống k8s context và cách kiểm tra

k3s là bản phân phối conformant — `OOMKilled`, `CrashLoopBackOff`, `restartCount`, CPU throttling
đều đến từ kubelet và cgroup nên có độ trung thực 100%. **Rủi ro không nằm ở k3s mà ở cấu hình
thu thập.** Bốn chỗ hay hụt:

1. **Prometheus không scrape kubelet/cAdvisor.** Lỗ thủng lớn nhất. Nếu chỉ scrape service của
   mình, sẽ **không có** `container_memory_working_set_bytes` và
   `container_cpu_cfs_throttled_seconds_total` → testcase #5 (OOMKill) và #6 (CPU throttling)
   mất hoàn toàn tín hiệu metric. Cần job scrape `https://<node>:10250/metrics/cadvisor`.
   Dùng `kube-prometheus-stack` qua Helm là nhanh nhất vì đã cấu hình sẵn.
2. **Event TTL mặc định 1 giờ.** `kubernetes-event-exporter` không phải tùy chọn.
3. **RBAC cho `k8sattributes`.** Cần ClusterRole `list/watch` trên `pods` và `namespaces`.
   Thiếu thì collector chạy bình thường nhưng không gắn được nhãn nào — **lỗi im lặng**.
4. **cgroup v2.** `stat -fc %T /sys/fs/cgroup/` phải trả về `cgroup2fs`.

**Chỉ tắt `traefik` và `servicelb`.** Giữ lại `metrics-server` (cần cho `kubectl top`) và
`local-path-provisioner` (cần cho PVC của ClickHouse và Prometheus).

Bốn lệnh nghiệm thu, chạy trước khi sang bước 1.2 — bất kỳ lệnh nào trả rỗng là k8s context đang thủng:

```bash
# 1. cAdvisor metrics có tới Prometheus không
curl -s 'localhost:9090/api/v1/query?query=container_memory_working_set_bytes{namespace="demo"}' | jq '.data.result | length'
curl -s 'localhost:9090/api/v1/query?query=container_cpu_cfs_throttled_seconds_total' | jq '.data.result | length'

# 2. kube-state-metrics có dữ liệu restart không
curl -s 'localhost:9090/api/v1/query?query=kube_pod_container_status_restarts_total' | jq '.data.result | length'

# 3. span có nhãn k8s không (chạy trong clickhouse-client)
#    SELECT ResourceAttributes['k8s.pod.name'], ResourceAttributes['k8s.deployment.name']
#    FROM otel_traces WHERE ServiceName='order' LIMIT 5;

# 4. event có được lưu ngoài etcd không — tạo OOM rồi tìm lại sau 90 phút
kubectl -n demo run oomtest --image=polinux/stress --limits=memory=32Mi -- stress --vm 1 --vm-bytes 200M
```

Lệnh số 4 quan trọng nhất và dễ bỏ qua nhất: nó xác nhận event đã được đẩy sang kho lưu trữ
lâu dài chứ không chỉ hiển thị tạm trong `kubectl get events`.

---

## 6. Đặc tả module RCA

### 6.1. incident-orchestrator

- Nhận webhook từ Alertmanager (`POST /webhook`).
- **Dedupe và gom nhóm**: một sự cố sinh ~20 alert. Nếu gọi LLM 20 lần thì vừa tốn tiền
  vừa ra 20 kết luận mâu thuẫn. Gom theo `(service, time_window 5 phút)`.
- State machine: `RECEIVED → COLLECTING → ANALYZING → DONE / FAILED`.
- Ghi `incident_id`, timeline, trạng thái.

Alert rule nên là SLO-based (error ratio, p99 latency, burn rate), không phải "CPU cao".
Chất lượng alert quyết định chất lượng context.

> **Ghi chú bổ sung — case #13 (nhiễu đồng thời):** key `(service, time_window)` chỉ đúng khi
> alert và root cause cùng một service. Case #13 kết hợp 2 fault độc lập ở 2 service khác nhau
> (VD #1 nổ alert ở `gateway`, #11 nổ alert ở `payment`) trong cùng cửa sổ 5 phút — với key hiện
> tại, chúng sẽ bị tách thành **2 incident riêng biệt** thay vì 1 incident có 2 nguyên nhân cần
> xếp hạng, làm mất ý nghĩa của case #13 như bài kiểm tra khả năng xếp hạng đa nguyên nhân (mục 7).
> Cần gom theo cụm liên thông trên dependency graph (các alert có service nằm trong cùng 1-2 hop
> của nhau) thay vì theo `service` đơn lẻ, khi thiết kế logic dedupe thật.

### 6.2. TelemetrySource — lớp cách ly bắt buộc

**Đây là quyết định kiến trúc quan trọng nhất cho tính tái sử dụng.** Toàn bộ SQL ClickHouse
phải nằm sau một interface. Production thật có thể dùng Datadog, Elastic, Grafana Cloud —
lúc đó chỉ viết một implementation mới, không đụng tới logic nén context.

Làm ngay từ Phase 2. Chi phí bây giờ: nửa ngày. Chi phí nếu bỏ qua: viết lại builder.

```go
package telemetry

type Window struct {
    From, To time.Time
    Service  string   // service phát alert, dùng làm điểm xuất phát
}

type SpanAgg struct {
    ServiceName, SpanName string
    P50, P95, P99         time.Duration
    Errors, Total         uint64
}

type LogLine struct {
    Timestamp time.Time
    TraceID   string
    Service   string
    Severity  string
    Body      string
}

type MetricDelta struct {
    Name     string
    Labels   map[string]string
    Current  float64
    Baseline float64   // cùng giờ hôm trước
    PctDiff  float64
}

type K8sEvent struct {
    Timestamp time.Time
    Namespace, Pod, Reason, Message string
    Count     int32
}

type TraceDetail struct {
    TraceID      string
    Spans        []Span
    CriticalPath []string   // tên span trên đường găng, đã tính sẵn
}

// Source là điểm cắm duy nhất giữa builder và kho dữ liệu.
type Source interface {
    TopErrorSpans(ctx context.Context, w Window, limit int) ([]SpanAgg, error)
    DependencyGraph(ctx context.Context, w Window, hops int) (Graph, error)
    ExemplarTraces(ctx context.Context, w Window, n int) ([]TraceDetail, error)
    LogsForTraces(ctx context.Context, traceIDs []string) ([]LogLine, error)
    LogsByService(ctx context.Context, w Window) ([]LogLine, error)   // fallback khi thiếu trace_id
    MetricDiffVsBaseline(ctx context.Context, w Window) ([]MetricDelta, error)
    K8sEvents(ctx context.Context, w Window) ([]K8sEvent, error)
    RecentChanges(ctx context.Context, w Window) ([]ChangeRecord, error)
}
```

Implementation ở POC: `clickhouse.Source` (traces + logs) và `prometheus.Source` (metrics),
gộp lại bằng một `CompositeSource`. Package `internal/telemetry/clickhouse` là **nơi duy nhất**
được phép chứa SQL.

Câu SQL mẫu:

```sql
-- TopErrorSpans
SELECT ServiceName, SpanName,
       quantile(0.99)(Duration) AS p99,
       countIf(StatusCode = 'Error') AS errors,
       count() AS total
FROM otel_traces
WHERE Timestamp BETWEEN {from:DateTime64} AND {to:DateTime64}
GROUP BY ServiceName, SpanName
ORDER BY errors DESC, p99 DESC
LIMIT {limit:UInt32};

-- LogsForTraces
SELECT l.Timestamp, l.TraceId, l.ServiceName, l.SeverityText, l.Body
FROM otel_logs l
WHERE l.TraceId IN {ids:Array(String)}
ORDER BY l.Timestamp;
```

**Nguyên tắc thu hẹp dần (narrowing), không quét toàn cluster.** Builder bắt đầu từ service phát
alert rồi mở rộng theo dependency graph 1–2 hop. Ở POC 6 service thì hai cách cho kết quả như nhau,
nhưng cách quét toàn bộ sẽ vỡ ở quy mô 200 service. Viết đúng ngay từ đầu.

**Fallback bắt buộc:** nếu `LogsForTraces` trả về rỗng (service legacy không có `trace_id` trong log),
tự động chuyển sang `LogsByService`. Kém chính xác hơn nhưng còn hơn không có gì.

### 6.3. Log clustering — implement Drain trong Go

Go không có port drain3 đủ trưởng thành, phải tự viết. Thuật toán Drain là cây phân tích
độ sâu cố định, khoảng 250–300 dòng Go:

1. Tokenize log message theo khoảng trắng.
2. Tầng 1 của cây: nhóm theo **số lượng token**.
3. Tầng 2..d: nhóm theo token thứ 1, 2, ... (bỏ qua token chứa chữ số — coi là biến).
4. Ở lá: so từng log cluster đã có bằng **similarity** = số vị trí token trùng / tổng token.
   Nếu ≥ `simThreshold` (0.4) thì gán vào cluster đó và cập nhật template
   (vị trí khác nhau → thay bằng `<*>`), ngược lại tạo cluster mới.

Interface:

```go
type Drain interface {
    Add(msg string) (templateID int, template string)
    Templates() []Template   // {ID, Template, Count, FirstSeen, LastSeen}
}
```

Mục tiêu: 100k dòng log rút gọn còn ~30 template + counter. Đây là bước quyết định việc
context pack có vừa token window hay không.

Builder chỉ đưa vào context pack các template có **tần suất tăng đột biến so với baseline**,
không phải template có count cao nhất (log INFO bình thường luôn nhiều nhất).

### 6.4. Context pack — schema có version

**Context pack là struct có version, không phải string ghép trực tiếp.** Builder sinh ra object,
một serializer riêng đổ thành prompt. Đổi backend không đụng prompt; đổi prompt không đụng query.

```go
type ContextPack struct {
    Version    string        `json:"version"`      // "v1", tăng khi đổi cấu trúc
    IncidentID string        `json:"incident_id"`
    Window     Window        `json:"window"`
    Alert      AlertSummary  `json:"alert"`

    // Năm phần, thứ tự này cũng là thứ tự ưu tiên khi cắt bớt vì token budget
    DependencyGraph Graph          `json:"dependency_graph"`
    MetricDiffs     []MetricDelta  `json:"metric_diffs"`
    Exemplars       []TraceDetail  `json:"exemplar_traces"`
    LogTemplates    []TemplateStat `json:"log_templates"`
    K8sContext      K8sContext     `json:"k8s_context"`

    TokenEstimate int      `json:"token_estimate"`
    Truncated     []string `json:"truncated"`   // phần nào đã bị cắt, để phân tích sau
}
```

Serializer nằm ở `internal/prompt`, có thể có nhiều phiên bản (`v1.tmpl`, `v2.tmpl`) để so sánh
hiệu quả prompt trong cùng một bộ dữ liệu — đây là biến số quan trọng cần đo.

### 6.5. Output format của LLM

Bắt buộc JSON có cấu trúc để evaluator chấm tự động:

```json
{
  "ranked_causes": [
    {
      "rank": 1,
      "service": "inventory",
      "cause": "connection_pool_exhausted",
      "confidence": 0.82,
      "evidence": ["trace:abc123 span wait_for_conn 450ms", "log_template:#7 count 1240"],
      "reasoning": "..."
    }
  ],
  "summary": "...",
  "suggested_actions": ["..."]
}
```

Parse bằng struct Go có `json` tag. Nếu LLM trả về JSON sai định dạng, retry 1 lần với
thông báo lỗi kèm theo, sau đó đánh dấu incident là `FAILED` — **không** tự sửa bằng regex,
vì tỷ lệ parse lỗi là một chỉ số cần đo và báo cáo.

### 6.6. LLM provider — free tier cho POC

**Đặc điểm workload: gọi rất ít lần nhưng mỗi lần rất nặng.**
Một vòng benchmark = 13 kịch bản × 5 lần lặp = 65 lần gọi × ~9.000 token (8k input + 1k output)
≈ **585.000 token**.

Hệ quả: **TPD (tokens/day) là ràng buộc, không phải RPM.** Đây là chỗ dễ chọn nhầm provider.
Ví dụ Groq `llama-3.3-70b-versatile` free tier có 30 RPM nhưng chỉ ~100K TPD — ở kích thước
context pack của dự án này chỉ đủ **~11 lần gọi/ngày**, không hết nổi một vòng 13 kịch bản.

| Provider | Điểm mạnh cho dự án này | Ghi chú |
|---|---|---|
| **Google AI Studio (Gemini)** | Context 1M token trên Flash, ~10–15 RPM free | Lựa chọn chính. Có lớp tương thích OpenAI |
| **Cerebras** | ~1M token/ngày → ~110 lần gọi | Đủ 2 vòng benchmark/ngày. Provider dự phòng |
| **OpenRouter** | ~30 model free qua 1 key, ~20 RPM/model | Dùng cho phần **so sánh nhiều model** |
| Groq | Nhanh, nhưng TPD thấp | Không phù hợp ở kích thước context này |

Cả ba đều cấp key không cần thẻ tín dụng.

**Quyền riêng tư không phải vấn đề ở đây.** Free tier thường dùng prompt để train model, nhưng
dữ liệu của dự án là telemetry tổng hợp từ service giả với lỗi do chính mình inject — không có
gì nhạy cảm. Rào cản chính của free tier không tồn tại trong bối cảnh này.

**Giới hạn free tier thay đổi gần như hàng tháng.** Các con số trên ghi nhận khoảng tháng 5–7/2026,
phải verify lại tại trang docs của provider trước khi chốt.

#### Interface và fallback

```go
// internal/llm/client.go
type Client interface {
    // trả về kết quả đã parse, số token đã dùng, và lỗi
    Analyze(ctx context.Context, prompt string) (RCAResult, Usage, error)
    Model() string   // ghi vào cột llm_model của bảng incidents
}

type Usage struct{ InputTokens, OutputTokens int }
```

Adapter: `gemini.Client`, `cerebras.Client`, `openrouter.Client`. Tất cả đều nói chuẩn
OpenAI-compatible nên mỗi adapter chỉ ~50 dòng.

Fallback chain khi gặp 429 hoặc timeout:

```go
type Chain struct{ clients []Client }   // thử lần lượt, ghi lại client nào thực sự trả kết quả
```

**Quan trọng:** khi fallback xảy ra, `llm_model` phải ghi model **thực sự đã trả lời**, không phải
model đầu chuỗi. Nếu không, số liệu so sánh giữa các model sẽ bị trộn lẫn một cách âm thầm.

#### So sánh nhiều model — biến ràng buộc thành đóng góp

Vì `context_pack` được lưu nguyên vẹn (mục 6.7), giai đoạn 2 chạy lại được với model khác
**trên cùng bộ dữ liệu**, không tốn thêm chi phí thu thập.

Câu hỏi nghiên cứu đáng trả lời: *chất lượng context pack có bù được cho model yếu hơn không?*
Đây là câu hỏi thực tế với mọi tổ chức muốn tự triển khai on-premise. Chạy tối thiểu 2 model
(một open-weight nhỏ, một frontier) trên cùng 13 pack và báo cáo chênh lệch.

#### Hai chỉ số phải đo riêng theo model

| Chỉ số | Vì sao quan trọng |
|---|---|
| **Tỷ lệ parse JSON lỗi** | Model nhỏ tuân thủ schema kém hơn rõ rệt. Đây là rào cản triển khai thực tế, phải báo cáo chứ không che bằng regex |
| **Latency mỗi lần gọi** | Ảnh hưởng tính khả thi khi dùng trong quy trình on-call thật |

Ưu tiên model có JSON mode / structured output gốc để giảm nhiễu từ vấn đề định dạng.

#### Chạy benchmark cuối cùng thì trả tiền

Free tier có thể bị 429 giữa chừng, throttle, hoặc đổi giới hạn giữa các lần chạy — làm hỏng
tính so sánh của số liệu. Chi phí trả tiền cho 585K token chỉ vài đô la.

**Quy tắc: free tier cho toàn bộ giai đoạn phát triển, trả tiền cho lần chạy đưa vào báo cáo.**

### 6.7. Bảng kết quả

```sql
CREATE TABLE incidents (
    incident_id      String,
    scenario_name    String,
    started_at       DateTime64(3),
    context_pack     String,          -- JSON, giữ nguyên để phân tích lại sau
    pack_version     String,
    prompt_version   String,
    llm_model        String,
    llm_output       String,          -- JSON
    ground_truth     String,          -- JSON, rỗng khi chạy production
    token_count      UInt32,
    latency_ms       UInt32,
    score_top1       Float32,
    score_mrr        Float32,
    human_verdict    Enum8('' = 0, 'correct' = 1, 'partial' = 2, 'wrong' = 3),
    human_note       String
) ENGINE = MergeTree ORDER BY (started_at, incident_id);
```

Hai cột `human_verdict` và `human_note` **phải có ngay từ đầu** dù POC chưa dùng: production thật
không có ground truth tự động, phải chuyển sang feedback thủ công từ SRE. Thêm cột vào bảng
đang chạy phiền hơn nhiều so với để sẵn.

Lưu `context_pack` nguyên vẹn cho phép chạy lại phân tích với prompt hoặc model khác trên cùng
dữ liệu — cần thiết cho phần so sánh trong báo cáo.

### 6.8. Khử trùng lặp — sáu loại, xử lý sai sẽ khiến LLM chỉ sai có hệ thống

Nguyên tắc: khử trùng lặp **không phải xóa bớt dữ liệu**, mà là đổi 1.240 bản sao lấy
1 bản gốc + số liệu thống kê về 1.240 bản đó.

| Loại | Biểu hiện | Cách xử lý |
|---|---|---|
| Log lặp nguyên văn | Một message xuất hiện 1.240 lần | Drain gộp thành template + count |
| **Lặp xuyên tầng** | Cùng lỗi được log ở inventory, order, gateway | Gộp theo `trace_id`, giữ span **sâu nhất** có lỗi, đánh dấu phần còn lại `propagated` |
| Span lặp do retry | `retry=2` sinh 3 span cho một lời gọi logic | Gộp theo `(trace_id, parent_span, target)`, giữ `attempt_count` |
| Metric trùng ngữ nghĩa | p99 tăng ở cả 3 tầng — cùng một hiện tượng | Trình bày thành **một chuỗi lan truyền**, không phải 3 bất thường độc lập |
| Exemplar giống nhau | 3 trace chậm nhất có hình dạng y hệt | Chọn đa dạng: 1 chậm nhất + 1 có error + 1 bình thường làm đối chứng |
| Alert lặp | 20 alert cho một sự cố | Xử lý ở orchestrator (mục 6.1) |

**Loại "lặp xuyên tầng" là nguy hiểm nhất.** Nếu chỉ đếm lỗi theo service, bảng xếp hạng sẽ là:

```
gateway 1240 | order 1240 | inventory 1240      ← vô nghĩa
```

Tệ hơn, `gateway` còn phục vụ route khác nên thường có tổng lỗi **cao nhất** → LLM chỉ vào gateway.
Sau khi gộp theo span sâu nhất:

```
inventory: root 1240, propagated 0
order:     root 0,    propagated 1240
gateway:   root 0,    propagated 1240      ← bảng này tự nó gần như đã trả lời
```

**Ràng buộc ngược — không được gộp mất hai thứ:**

1. **Thời điểm xuất hiện đầu tiên tại mỗi service.** Thứ tự `inventory 10:03:12.140 →
   order .142 → gateway .145` là tín hiệu nhân quả mạnh nhất trong cả pack. Gộp thành
   "cả 3 service đều lỗi" là xóa mất câu trả lời.
2. **Hình dạng theo thời gian.** 1.240 lỗi dồn trong 10 giây (sập đột ngột) khác hoàn toàn
   1.240 lỗi rải đều 5 phút (cạn tài nguyên từ từ) — cùng `count`, hai chẩn đoán khác nhau.

Vì vậy mỗi cụm sau khi gộp phải giữ: `count`, `first_seen`, `last_seen`, phân bố theo service,
và `rate_shape` (hoặc histogram thô 10 bin).

```json
{
  "template_id": 7,
  "template": "failed to acquire connection: <*>",
  "count": 1240,
  "first_seen": "10:03:12.140",
  "last_seen": "10:08:02.331",
  "by_service": {"inventory": 1240},
  "baseline_count": 0,
  "rate_shape": "sustained"
}
```

---

## 7. Bộ testcase và ground truth

Mỗi kịch bản là một file YAML, fault-controller đọc và thực thi. Chạy full suite = vòng lặp qua thư mục.

```yaml
name: db_pool_exhaustion
steps:
  - {target: inventory, action: set_config, params: {pool_size: 2}}
  - {action: wait, duration: 300s}
  - {target: inventory, action: reset}
ground_truth:
  service: inventory
  cause: connection_pool_exhausted
  distractors: [postgres_slow, traffic_spike]
```

| # | Kịch bản | Cách inject | Root cause thật | Độ khó |
|---|---|---|---|---|
| 1 | Timeout cascade | `latency` ở inventory | inventory chậm, symptom ở gateway | Dễ |
| 2 | Bad deploy | `error.probability=0.3` | version mới | Dễ |
| 3 | DB pool exhaustion | `pool_size=2` + tăng tải | thiếu connection, không phải DB chậm | Khó |
| 4 | N+1 query | bật `n_plus_one` | pattern query, thấy qua span count | Trung bình |
| 5 | OOMKill → CrashLoop | `mem_leak` + memory limit 64Mi | leak, không phải traffic | Trung bình |
| 6 | CPU throttling | hạ CPU limit xuống 100m | limit sai, không phải code chậm | Khó |
| 7 | Cache down | scale redis về 0 | mất cache → DB quá tải (lỗi lan) | Khó |
| 8 | Retry storm | retry=3 ở cả 3 tầng + lỗi ở đáy | amplification 27x | Rất khó |
| 9 | Consumer lag | scale notify-worker về 0 | queue backlog, API vẫn 200 OK | Trung bình |
| 10 | Poison message | publish payload sai schema | 1 message làm chết consumer | Trung bình |
| 11 | Rate limit bên thứ ba | mock trả 429 | dependency ngoài | Dễ |
| 12 | Config sai | xóa env var khi restart | lỗi lúc khởi động | Trung bình |
| 13 | **Nhiễu đồng thời** | chạy #1 và #11 cùng lúc | 2 nguyên nhân độc lập | Rất khó |

Case 13 là bài kiểm tra thật sự: hệ thống có xếp hạng được nguyên nhân, hay chỉ liệt kê mọi bất thường?

**Metric đánh giá:** top-1 accuracy, MRR, và tỷ lệ chọn nhầm distractor.

---

## 8. Ngân sách tài nguyên

| Nhóm | Thành phần | CPU request | RAM limit |
|---|---|---|---|
| workload | 6 service chain | 300m | 384Mi |
| | postgres + redis | 350m | 384Mi |
| telemetry | otel-collector | 300m | 256Mi |
| | clickhouse | 1000m | 1Gi |
| | prometheus + alertmanager | 350m | 576Mi |
| | grafana | 100m | 192Mi |
| | jaeger (tùy chọn) | 100m | 200Mi |
| | kube-state-metrics + event-exporter | 100m | 128Mi |
| rca | orchestrator + context-builder | 300m | 384Mi |
| chaos | fault-controller + evaluator + k6 | 600m | 320Mi |
| hệ thống | k3s control plane | 1000m | 768Mi |
| **Tổng** | ~21 pod | **≈ 4.5 CPU** | **≈ 4.6Gi** |

Đỉnh ngắn hạn 6–7 CPU khi ClickHouse merge partition hoặc context-builder chạy query.
Trên máy 8 core vẫn thoải mái.

Nếu cần ép xuống dưới 3Gi: bỏ Jaeger, ClickHouse còn 768Mi, Prometheus retention 6h.

---

## 9. Cấu trúc thư mục đề xuất

Mono-repo Go, một `go.mod` ở gốc, nhiều binary trong `cmd/`. Package dùng chung
(`pkg/otelinit`, `pkg/telemetry`) tránh lặp code khởi tạo OTel giữa các module.

```
.
├── go.mod                     # module github.com/<org>/llm-rca
├── cmd/
│   ├── chain/                 # 1 binary cho cả 6 service của service chain
│   ├── orchestrator/
│   ├── builder/
│   ├── faultctl/
│   └── evaluator/
├── internal/
│   ├── chain/
│   │   ├── config/            # parse YAML config
│   │   ├── handler/           # generic route handler
│   │   └── faults/            # fault middleware + /_fault endpoint
│   ├── telemetry/
│   │   ├── source.go          # interface Source (mục 6.2)
│   │   ├── clickhouse/        # NƠI DUY NHẤT chứa SQL
│   │   ├── prometheus/
│   │   ├── fixture/           # đọc testdata/, dùng khi test offline
│   │   └── composite.go
│   ├── builder/
│   │   ├── pack.go            # ContextPack struct có version
│   │   ├── narrow.go          # thu hẹp dần theo dependency graph
│   │   ├── dedup.go           # sáu quy tắc mục 6.8
│   │   ├── criticalpath.go
│   │   └── testdata/scenarios/<name>/   # fixture telemetry thô
│   ├── drain/                 # implement Drain (mục 6.3)
│   ├── prompt/                # serializer ContextPack → prompt, có version
│   ├── llm/                   # client interface, adapter theo provider, fallback chain, parse JSON
│   ├── incident/              # state machine, dedupe
│   └── store/                 # bảng incidents
├── pkg/otelinit/              # OTel SDK setup + logger có trace_id, dùng chung
├── deploy/
│   ├── namespaces/
│   ├── demo/                  # 6 Deployment + 6 ConfigMap + Services
│   ├── observability/         # collector, clickhouse, prometheus, grafana, ksm
│   ├── rca/
│   ├── chaos/
│   └── networkpolicy/         # cách ly chaos ↔ observability
├── scenarios/                 # 13 file YAML
├── loadtest/                  # k6 script
└── CLAUDE.md
```

---

## 10. Thứ tự triển khai — hai giai đoạn

Dự án chia làm **hai giai đoạn có ranh giới là file JSON `ContextPack`**.

Lý do tách: giai đoạn 1 **tất định** (cùng input → cùng output, test được bằng assertion),
giai đoạn 2 **ngẫu nhiên** (chỉ đo được bằng thống kê qua nhiều lần chạy). Trộn hai thứ lại
là lý do khiến nhiều dự án LLM không biết lỗi nằm ở đâu khi kết quả kém.

```
telemetry thô ──[GĐ1: thu thập, khử trùng lặp, nén]──→ ContextPack (JSON)
ContextPack ───[GĐ2: prompt, gọi LLM, parse]────────→ RCAResult (JSON)
```

Ranh giới này cũng là ranh giới quy trách nhiệm khi kết quả sai:
- Bằng chứng root cause **không có** trong ContextPack → lỗi giai đoạn 1
- Bằng chứng **có mặt** mà LLM vẫn chỉ sai → lỗi giai đoạn 2 (prompt hoặc model)

### Phase 0 — lát cắt mỏng (1 ngày, làm TRƯỚC giai đoạn 1)

**Đừng hoàn thiện 100% giai đoạn 1 rồi mới chạm vào LLM.** Rủi ro là dựng được ContextPack
đúng về kỹ thuật nhưng LLM không dùng được.

Làm một lát cắt xuyên cả hai giai đoạn: 1 kịch bản (#1 timeout cascade), builder viết tay
context pack cũng được, gọi LLM một lần, đọc output. Mục đích không phải để đúng, mà để
biết ContextPack nên có hình dạng nào. Rất có thể phát hiện LLM cần **ít** context hơn
dự kiến nhưng cần **đúng loại** context hơn nhiều.

Sau bước này mới quay về làm giai đoạn 1 cho đầy đủ.

---

### GIAI ĐOẠN 1 — thu thập và dựng context

**1.1. Hạ tầng và đường ống dữ liệu**

Service chain (6 pod) + otel-collector + ClickHouse + Prometheus + Grafana trên k3s.
Chưa làm fault-controller — inject thủ công bằng `curl` vào `/_fault`.

Nghiệm thu: Grafana thấy trace 4 tầng đầy đủ; câu SQL join `otel_logs` với `otel_traces`
theo `trace_id` trả về khác rỗng; 4 lệnh kiểm tra ở mục 5.1 đều pass.

Ràng buộc: **không hardcode giả định single node.** PVC dùng `local-path` thay vì `hostPath`,
không đặt `nodeName` cứng. Làm đúng thì chuyển sang k3d multi-node sau chỉ mất một buổi.

**1.2. Fixture — tách builder khỏi cluster**

Chạy mỗi kịch bản một lần trên k3s, dump toàn bộ telemetry thô của window ra
`testdata/scenarios/<name>/`. Từ đó phát triển builder hoàn toàn offline.

Đây là chỗ interface `telemetry.Source` trả cổ tức lần thứ hai:

```go
// internal/telemetry/fixture/source.go — cùng interface, đọc từ file JSON
type Source struct{ dir string }
```

Lợi ích: test chạy 200ms thay vì 5 phút, kết quả tất định, không cần cluster.

**1.3. Builder đầy đủ**

Năm phần của ContextPack (mục 6.4), sáu quy tắc khử trùng lặp (mục 6.8), Drain (mục 6.3),
thu hẹp dần theo dependency graph (mục 6.2).

**1.4. Fault-controller và bộ 13 kịch bản**

Đọc scenario YAML, gọi `/_fault` hoặc patch k8s resource, ghi ground truth.

**Nghiệm thu giai đoạn 1 — ba chỉ số, chưa động tới LLM:**

| Chỉ số | Ý nghĩa | Mục tiêu |
|---|---|---|
| **Evidence recall** | Bằng chứng root cause có mặt trong pack không | 13/13 kịch bản |
| **Noise ratio** | Tỷ lệ service/log không liên quan lọt vào pack | Càng thấp càng tốt |
| **Token count** | Kích thước pack | < 8000 mỗi kịch bản |

Kiểm tra bằng assertion khai báo trong chính file kịch bản, không so khớp golden file nguyên văn
(timestamp và trace ID đổi mỗi lần chạy nên so khớp chính xác sẽ hỏng liên tục):

```yaml
name: db_pool_exhaustion
ground_truth:
  service: inventory
  cause: connection_pool_exhausted
  distractors: [postgres_slow, traffic_spike]

context_assertions:
  must_include_service: [inventory, postgres]
  must_rank_in_top3: inventory
  must_include_log_template_matching: "(?i)connection pool|acquire.*timeout|wait.*conn"
  must_include_metric_delta:
    - {name_matches: "db_pool_wait", min_pct_diff: 100}
  must_not_include: ["fault", "inject", "chaos", "_fault"]
  max_tokens: 8000
  max_services_included: 6
```

Dòng `must_not_include` là **bài kiểm tra tự động cho ràng buộc cách ly ở mục 2.4**. Nếu fault
middleware vô tình để lộ dấu vết, test đỏ ngay thay vì phát hiện sau khi đã công bố kết quả.

Chạy bằng `go test ./internal/builder/... -run TestScenarios` trên toàn bộ 13 fixture,
mỗi lần sửa builder.

---

### GIAI ĐOẠN 2 — suy luận bằng LLM

**2.1.** Serializer `ContextPack` → prompt (`internal/prompt`, có version).
**2.2.** `internal/llm`: interface `Client`, adapter Gemini/Cerebras/OpenRouter, fallback chain,
parse JSON output, retry 1 lần nếu sai định dạng (mục 6.6).
**2.3.** Evaluator: top-1 accuracy, MRR, tỷ lệ chọn nhầm distractor, **tỷ lệ parse lỗi và
latency tách riêng theo model**.
**2.4.** Chạy lặp **nhiều lần mỗi kịch bản** (tối thiểu 5) để đo variance — output LLM
không tất định, một lần chạy không nói lên điều gì.
**2.5.** Chạy lại toàn bộ với **ít nhất 2 model** (một open-weight nhỏ, một frontier) trên cùng
bộ context pack đã lưu, để trả lời câu hỏi *chất lượng context có bù được cho model yếu hơn không*.

Nghiệm thu: bảng kết quả 13 kịch bản × N lần chạy × M model, kèm độ lệch chuẩn.
Lần chạy đưa vào báo cáo dùng API trả tiền, không dùng free tier (mục 6.6).

Vì `context_pack` được lưu nguyên vẹn trong bảng `incidents`, giai đoạn 2 chạy lại được
với prompt hoặc model khác **trên cùng dữ liệu** — cần thiết cho phần so sánh trong báo cáo.

### Phase mở rộng (nếu còn thời gian)

- k3d 1 server + 2 agent → thêm 3 kịch bản node-level (mục 11)
- OpenSearch làm log backend thứ hai → chứng minh khả năng đa backend qua `telemetry.Source`
- So sánh 2–3 prompt version trên cùng bộ fixture

---

## 11. Giới hạn đã biết (nêu thẳng trong báo cáo)

Cố tình bỏ: Kafka, service mesh, multi-node cluster, tail sampling, HPA.

Hệ quả: **không** demo được sự cố đặc thù của mesh (mTLS cert hết hạn, sidecar OOM,
retry budget của Envoy) và sự cố phân tán nhiều node (network partition, node pressure).

Đây là giới hạn về phạm vi, không phải về phương pháp. Nếu hệ thống tìm đúng root cause trong
13 kịch bản có nhiễu và distractor, việc thiếu mesh không làm suy yếu luận điểm chính.

**Nhóm node-level** (node NotReady, network partition, eviction) không tái tạo được trên single node.
Nếu ở **Phase mở rộng** còn thời gian, chuyển sang **k3d 1 server + 2 agent** (~600Mi thêm) để mở khóa 3 kịch bản
mới: node NotReady (độ trễ 5 phút giữa nguyên nhân và triệu chứng — rất khó cho con người),
node pressure eviction, network partition. Manifest giữ nguyên hoàn toàn, chỉ đổi cách dựng cluster.

---

## 12. Đường chuyển giao lên hạ tầng production

Mức độ tái sử dụng phân bố không đều. Thiết kế ở mục 6 là để bảo vệ phần có giá trị cao nhất.

| Module | Tái sử dụng | Ghi chú |
|---|---|---|
| `internal/builder`, `internal/drain`, `internal/prompt` | ~95% | Độc lập hoàn toàn với hạ tầng |
| `internal/incident` (orchestrator) | ~90% | Chỉ cần adapter nếu nguồn alert khác Alertmanager |
| Prompt + output schema | 100% | Phần giá trị nhất và bền nhất |
| `internal/telemetry/clickhouse` | 0% | Đúng như thiết kế — viết implementation mới cho backend khác |
| `cmd/chain`, `cmd/faultctl` | 0% | Là môi trường thử nghiệm, không phải sản phẩm |
| Manifest k8s | ~60% | Thiếu HA, PDB, secret management thật |

**Bốn chỗ sẽ vỡ, và cách đã phòng ngừa:**

1. *Backend khác* → interface `telemetry.Source` (mục 6.2)
2. *Quy mô 200 service, sampling 1%* → nguyên tắc thu hẹp dần thay vì quét toàn cluster.
   Đây vẫn là giới hạn phương pháp thật, không giải quyết hết được bằng thiết kế
3. *Không có ground truth* → cột `human_verdict`, `human_note` (mục 6.7)
4. *Log legacy thiếu `trace_id`* → fallback `LogsByService` (mục 6.2)

**Phạm vi khẳng định trong báo cáo — nêu rõ, đừng khẳng định quá tay:**

POC chứng minh: *với context pack cấu trúc X, LLM đạt top-1 accuracy Y% trên 13 kịch bản có distractor.*

POC **không** chứng minh hệ thống hoạt động ở quy mô 200 service — đó là bài toán retrieval và
ranking ở quy mô lớn, cần nghiên cứu riêng. Phân định rõ làm báo cáo mạnh hơn, vì đây chính là
câu hỏi phản biện sẽ được đặt ra.

---

## 13. Quy ước code Go

- Một `go.mod` ở gốc, binary trong `cmd/`, logic trong `internal/`, code dùng chung trong `pkg/`.
- Mọi hàm chạm I/O nhận `context.Context` làm tham số đầu tiên — cần cho trace propagation.
- Log bằng `log/slog` với OTel bridge, luôn dùng biến thể có context:
  `slog.InfoContext(ctx, "...")`. Dùng `slog.Info` (không context) là mất `trace_id`.
- Error wrap bằng `fmt.Errorf("...: %w", err)`, không nuốt lỗi.
- Dockerfile multi-stage, `CGO_ENABLED=0`, base image `gcr.io/distroless/static`.
  Binary Go tĩnh giúp giữ memory limit 64Mi khả thi.
- Test: ưu tiên table-driven. `internal/drain` và `internal/builder` phải có unit test —
  đây là hai chỗ logic phức tạp nhất và không thể verify bằng mắt.