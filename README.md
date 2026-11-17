# llm-rca — Phân tích nguyên nhân gốc (RCA) bằng LLM cho microservices trên Kubernetes

Hệ thống dùng LLM đọc trace, log, metric và k8s event của một sự cố, rồi đưa ra danh sách
**nguyên nhân gốc được xếp hạng**. Điểm khác biệt là độ chính xác được **đo định lượng**: hệ
thống tự bơm lỗi thật vào một chuỗi microservice mẫu, ghi lại đáp án đúng (ground truth), rồi
chấm điểm câu trả lời của LLM bằng top-1 accuracy và MRR.

Đây là một POC nghiên cứu, không phải sản phẩm.

## Kiến trúc

```
┌─ namespace: demo ──────────────────────────────────────────────────────┐
│  k6 (tải nền 20 rps) → gateway → order ─┬→ inventory → postgres        │
│                                         ├→ payment → thirdparty-mock   │
│                                         ├→ redis (cache)               │
│                                         └→ redis stream → notify-worker│
└────────────────────────────────────────────────────────────────────────┘
            │ OTLP (trace/log/metric)              │ Prometheus scrape
            ▼                                      ▼
┌─ namespace: observability ─────────────────────────────────────────────┐
│  otel-collector → ClickHouse (otel_traces, otel_logs)                  │
│  Prometheus + Alertmanager (kube-prometheus-stack)                     │
│  event-exporter → k8s Event của namespace demo → ClickHouse            │
│  Jaeger UI, Coroot (giao diện quan sát)                                │
│                                                                        │
│  orchestrator: Alertmanager webhook → gom alert thành incident         │
│     → dựng ContextPack → gọi LLM (Gemini) → lưu kết quả vào ClickHouse │
└────────────────────────────────────────────────────────────────────────┘

┌─ namespace: fault-manager (cách ly bằng NetworkPolicy) ────────────────┐
│  faultctl: đọc kịch bản YAML → bơm lỗi thật → ghi ground truth riêng   │
│  evaluator: đối chiếu kết quả LLM với ground truth → top-1 / MRR       │
└────────────────────────────────────────────────────────────────────────┘
```

### Nguyên tắc thiết kế

- **Cách ly tuyệt đối:** `fault-manager` và ground truth không bao giờ lộ ra phía telemetry. Nếu
  LLM "thấy" dấu vết của việc lỗi bị cố ý tạo ra thì phép đo mất ý nghĩa.
- **Một kho dữ liệu duy nhất:** cả trace lẫn log đều nằm trong ClickHouse.
- **Hai giai đoạn, ranh giới là `ContextPack`:** giai đoạn thu thập là tất định và được kiểm tra
  bằng assertion; giai đoạn LLM suy luận là ngẫu nhiên và được đo bằng thống kê. Khi kết quả sai,
  `context_assertions` cho biết ngay bằng chứng *thiếu trong pack* (lỗi thu thập) hay *có mà
  LLM vẫn chọn sai* (lỗi prompt/model).
- **Đa dạng loại lỗi quan trọng hơn số lượng service:** 6 service, 13 kiểu lỗi khác nhau.

## Luồng một sự cố

1. `faultctl` (Job) bơm lỗi: gọi endpoint nội bộ `/_fault`, patch resource k8s, hoặc publish
   message hỏng; đồng thời ghi ground truth.
2. Lỗi biểu hiện thật dưới tải k6 → sinh telemetry thật.
3. Prometheus phát hiện vi phạm SLO (p99 > 1s, error rate > 10%, 0 replica sẵn sàng) →
   Alertmanager gửi webhook cho orchestrator.
4. Orchestrator gom alert theo dependency graph, chờ 20s, dựng `ContextPack` gồm dependency
   graph, metric diff so với baseline, exemplar trace, log template (Drain) và k8s event.
5. LLM trả về JSON `ranked_causes` (service, nguyên nhân, độ tin cậy, bằng chứng).
6. `evaluator` khớp incident với ground truth, tính top-1/MRR và chạy `context_assertions`.

## 13 kịch bản lỗi

Định nghĩa trong [deploy/fault-manager/scenarios/](deploy/fault-manager/scenarios/):

| Kịch bản | Cách bơm lỗi |
|---|---|
| `timeout_cascade` | Tăng latency ở inventory, triệu chứng lộ ra ở gateway |
| `bad_deploy` | payment trả lỗi 500 với xác suất 0.3 |
| `db_pool_exhaustion` | `pool_size=2` + tải dồn |
| `n_plus_one_query` | Bật truy vấn lặp N+1 |
| `oom_crashloop` | Rò rỉ bộ nhớ → OOMKill → CrashLoop |
| `cpu_throttling` | Hạ CPU limit + đốt CPU |
| `cache_down` | Scale redis về 0 |
| `retry_storm` | Retry ở 2 tầng khuếch đại lỗi ở tầng dưới |
| `consumer_lag` | Scale notify-worker về 0, stream bị dồn |
| `poison_message` | Publish message thiếu field bắt buộc |
| `third_party_rate_limit` | thirdparty-mock trả 429 |
| `config_error` | Xoá `CONFIG_PATH` khỏi Deployment |
| `concurrent_faults` | Hai lỗi độc lập cùng lúc (xếp hạng đa nguyên nhân) |

## Cấu trúc repo

```
cmd/                   entrypoint + Dockerfile của từng thành phần
  chain/               service Go dùng chung (gateway, order, inventory, thirdparty-mock),
                       hành vi do ConfigMap quyết định
  payment-java/        service payment (Java)
  notify-py/           consumer notify-worker (Python)
  orchestrator/        pipeline RCA: webhook → ContextPack → LLM
  faultctl/            bơm lỗi theo kịch bản
  evaluator/           chấm điểm kết quả LLM
  eventwatcher/        thu thập k8s Event (client-go informer)
  rcactl/              CLI vận hành
internal/              logic dùng chung của các binary Go
  chain/               config, handler, fault middleware, consumer
  builder/ contextpack/ drain/ prompt/ llm/ telemetry/ incident/
  scenario/ groundtruth/ jobrunner/
pkg/otelinit/          khởi tạo OpenTelemetry SDK + logger gắn trace_id
deploy/                manifest Kubernetes
  namespaces/ demo/ observability/ rca/ fault-manager/ networkpolicy/
```

Các binary Go dùng chung một `go.mod` ở thư mục gốc, nên khi build phải dùng repo root làm
context:

```bash
docker build -f cmd/orchestrator/Dockerfile -t <image> .
docker build -t <image> cmd/payment-java     # Java/Python: dùng thư mục service làm context
```

## Vận hành

**Benchmark** bằng `rcactl` (dùng kubeconfig cục bộ):

```bash
go build -o rcactl ./cmd/rcactl
./rcactl full-run deploy/fault-manager/scenarios/timeout_cascade.yaml   # inject + validate
./rcactl inject   <scenario-file>
./rcactl validate <scenario-file>
```

**Điều tra thủ công** qua HTTP API của orchestrator:

```bash
kubectl port-forward svc/orchestrator -n observability 8080:8080
./rcactl list
./rcactl show    <incident-id>
./rcactl scan    <incident-id> service|k8s
./rcactl analyze <incident-id>
```

**Giao diện** (NodePort):

| Thành phần | Port |
|---|---|
| Coroot | 30880 |
| Jaeger UI | 30686 |
| ClickHouse (native / HTTP) | 30900 / 30823 |

## Tech stack

Go 1.26, Java, Python · OpenTelemetry · ClickHouse · Prometheus/Alertmanager · Jaeger · Coroot ·
k6 · Redis · PostgreSQL · Gemini (endpoint tương thích OpenAI) · Kubernetes (Calico)
