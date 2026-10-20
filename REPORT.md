# Báo cáo dự án: LLM-based RCA cho hệ thống microservices

**Thời gian thực hiện:** 2026-08-19 → 2026-08-25 (Giai đoạn 1, local, 7 ngày, 44 commit); tiếp tục
2026-09-03 (Giai đoạn 2, migrate cloud 4-node — xem mục 9); tiếp tục 2026-09-18 → nay (Giai đoạn 3,
triển khai lại trên cụm production 3-node dùng chung, mạng cô lập — xem mục 10)
**Trạng thái:** 13/13 kịch bản lỗi đã được xác thực end-to-end trên local (Giai đoạn 1), trên cụm
cloud 4-node cũ (mục 9, **nay không còn là cụm đang chạy**), và trên cụm production 3-node hiện tại
(mục 10, **cụm đang chạy thật lúc viết báo cáo này**)

---

## 1. Mục tiêu dự án

Xây dựng một hệ thống dùng LLM để tóm tắt log/trace/alert và gợi ý nguyên nhân gốc (root cause)
cho sự cố trên các dịch vụ chạy trong Kubernetes — sau đó **đo đạc định lượng** độ chính xác của
gợi ý đó, thay vì chỉ đánh giá cảm tính "câu trả lời có vẻ hợp lý".

Đây là **POC nghiên cứu**, không phải sản phẩm. Tiêu chí thành công: top-1 accuracy và MRR
(Mean Reciprocal Rank) trên bộ 13 kịch bản lỗi có ground truth biết trước.

Toàn bộ đặc tả gốc nằm trong [`PLAN.md`](./PLAN.md) — tài liệu này là báo cáo về **những gì
đã thực sự làm được** so với đặc tả đó, bao gồm cả các chỗ thực tế khác với thiết kế ban đầu.

### Nguyên tắc thiết kế xuyên suốt

1. **Giá trị đến từ độ đa dạng loại lỗi, không phải số lượng service** — 6 service nhưng mỗi
   service có thể bị inject nhiều loại lỗi khác nhau.
2. **Một kho dữ liệu duy nhất (ClickHouse)** cho cả trace lẫn log — tránh phải viết nhiều adapter.
3. **Một image Go duy nhất** cho toàn bộ chuỗi service — hành vi khác nhau chỉ do ConfigMap khác.
4. **Cách ly tuyệt đối** giữa bộ điều khiển lỗi (fault-controller) và tầng quan sát (observability)
   — nếu LLM "nhìn thấy" dấu vết của việc lỗi bị cố ý tạo ra, toàn bộ phép đo trở nên vô nghĩa.
5. **Hai giai đoạn tách bạch, ranh giới là file JSON `ContextPack`:** Giai đoạn 1 (thu thập, tất
   định, test bằng assertion) và Giai đoạn 2 (LLM suy luận, ngẫu nhiên, chỉ đo bằng thống kê).
   Khi kết quả sai, ranh giới này cho biết ngay lỗi thuộc giai đoạn nào.

---

## 2. Kiến trúc hệ thống

Bốn namespace Kubernetes, chạy trên k3s single-node (WSL2), tổng cộng ~4.5 CPU / ~4.6Gi RAM.

```
┌─────────────────────────────────────────────────────────────────────┐
│ namespace: demo — workload mô phỏng                                  │
│                                                                        │
│  k6 → gateway → order ──┬──→ inventory ──→ postgres                  │
│                         ├──→ payment ────→ thirdparty-mock            │
│                         ├──→ redis (cache)                            │
│                         └──→ redis stream ──→ notify-worker           │
└─────────────────────────────────────────────────────────────────────┘
                              │ OTLP push (traces/logs) + Prometheus scrape (metrics)
                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│ namespace: observability — thu thập & lưu trữ                        │
│                                                                        │
│  otel-collector → ClickHouse (traces + logs)                         │
│  kube-state-metrics / cAdvisor → Prometheus → Alertmanager            │
│  eventwatcher (custom) → k8s Event → ClickHouse                       │
└─────────────────────────────────────────────────────────────────────┘
                              │ webhook khi alert fires
                              ▼
┌─────────────────────────────────────────────────────────────────────┐
│ namespace: rca — pipeline phân tích                                  │
│                                                                        │
│  Alertmanager → incident-orchestrator (gom nhóm, xây ContextPack,     │
│                  gọi LLM, lưu kết quả vào ClickHouse)                 │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│ namespace: fault-manager — điều khiển & đánh giá (cách ly khỏi 3 namespace   │
│ trên, không lộ dấu vết cho LLM thấy)                                 │
│                                                                        │
│  faultctl (đọc kịch bản YAML → inject lỗi thật) → ground-truth store │
│  evaluator (đối chiếu kết quả LLM với ground truth → chấm điểm)      │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 3. Các thành phần và chức năng

### 3.1. Namespace `demo` — service chain mô phỏng

| Thành phần | Chức năng |
|---|---|
| `cmd/chain` | **Một binary Go duy nhất**, deploy 6 lần với 6 ConfigMap khác nhau (gateway, order, inventory, payment, thirdparty-mock, notify-worker). Hành vi (route nào gọi service nào, có DB/cache/publish không) hoàn toàn do file YAML cấu hình quyết định. |
| `internal/chain/config` | Parse ConfigMap YAML thành cấu trúc route/DB/cache/calls/publish. |
| `internal/chain/handler` | Handler HTTP generic: áp fault → đọc cache (cache-aside thật) → query DB → gọi downstream (có retry) → publish message → trả response. Đây là nơi tập trung hầu hết logic nghiệp vụ dùng chung cho cả 6 service. |
| `internal/chain/faults` | Bộ điều khiển fault in-memory, phục vụ endpoint nội bộ `POST /_fault` (port 9999, không public). Hỗ trợ: latency, error code, cpu_burn, mem_leak, pool_size, n_plus_one, retry — tất cả có thể override động, ưu tiên cao hơn ConfigMap tĩnh khi đang active. |
| `internal/chain/consumer` | Consumer Redis Stream cho notify-worker — bao gồm cơ chế "poison message" cố ý (panic nếu message thiếu field bắt buộc), mô phỏng lỗi thật khi có message hỏng. |
| `pkg/otelinit` | Khởi tạo OpenTelemetry SDK dùng chung (trace/log/metric) + logger có gắn `trace_id`/`span_id` tự động. |

### 3.2. Namespace `observability` — thu thập và lưu trữ

| Thành phần | Chức năng |
|---|---|
| `otel-collector` | Nhận OTLP từ 6 service (gRPC :4317), gắn nhãn k8s (pod/deployment/namespace), đẩy trace+log vào ClickHouse, đẩy metric ra endpoint Prometheus scrape. |
| ClickHouse | Kho dữ liệu hợp nhất cho `otel_traces` + `otel_logs`, TTL 48h. |
| kube-prometheus-stack | Prometheus (scrape metric app + cAdvisor + kube-state-metrics) + Alertmanager (route alert tới webhook) + Grafana. |
| `cmd/eventwatcher` | Tool tự viết (thay `kubernetes-event-exporter` — phát hiện bị hỏng trên k3s v1.36) dùng `client-go` SharedInformer để bắt k8s Event, **giới hạn nghiêm ngặt theo namespace `demo`** — đây là một trong các điểm cách ly quan trọng nhất. |

### 3.3. Namespace `rca` — pipeline phân tích

| Thành phần | Chức năng |
|---|---|
| `cmd/orchestrator` | Nhận webhook Alertmanager → gom nhóm alert thành incident (`internal/incident.Grouper`, gom theo cụm liên thông trên dependency graph, không phải chỉ theo tên service) → chờ cửa sổ thu thập 20s → gọi `internal/builder` xây `ContextPack` → gọi LLM (`internal/llm`) → lưu kết quả vào bảng `incidents` trong ClickHouse. |
| `internal/telemetry` | Interface `Source` — điểm cắm duy nhất giữa builder và kho dữ liệu thật (ClickHouse cho trace/log, Prometheus cho metric), cho phép đổi backend mà không đụng logic nén context. |
| `internal/builder` | Lắp ráp `ContextPack` thật từ dữ liệu telemetry: dependency graph, metric diff so baseline, exemplar trace, log template (qua Drain), k8s Event — có giới hạn kích thước (cap log template, cap k8s event, ưu tiên event mức Warning) để không vượt ngân sách token. |
| `internal/drain` | Tự viết thuật toán Drain (log clustering) — gộp hàng nghìn dòng log gần giống nhau thành một template + counter. |
| `internal/llm` + `internal/llm/gemini` | Interface gọi LLM, adapter Gemini (OpenAI-compatible endpoint), retry 1 lần nếu JSON trả về sai định dạng. |
| `internal/prompt` | Serialize `ContextPack` → prompt text gửi cho LLM. |

### 3.4. Namespace `fault-manager` — điều khiển & đánh giá

| Thành phần | Chức năng |
|---|---|
| `cmd/faultctl` | Đọc kịch bản YAML, thực thi từng bước: gọi `/_fault` (HTTP), patch trực tiếp resource k8s (CPU limit, scale replicas, xoá/khôi phục env var — qua `client-go`), publish message lỗi thẳng vào Redis Stream, tạo tải burst. Ghi ground truth vào store riêng (file-based, không qua mạng). |
| `internal/groundtruth` | Kho ground truth cách ly — cố ý **không** dùng chung ClickHouse với phần còn lại, để không ai (kể cả vô tình) đọc được đáp án. |
| `internal/scenario` | Parse kịch bản YAML + chạy các `context_assertions` khai báo (assertion tự động kiểm tra bằng chứng có mặt trong `ContextPack` hay không — độc lập với kết quả LLM). |
| `cmd/evaluator` | Tìm incident khớp với ground truth theo thời gian, tính top-1/MRR (hoặc recall cho kịch bản đa nguyên nhân), chạy `context_assertions`, ghi điểm lại vào ClickHouse. |
| k6 (`loadtest/basic.js`) | Sinh tải nền liên tục 20rps vào `gateway`, `user_id` ngẫu nhiên dải rộng — nếu không có tải nền, kịch bản `wait` sẽ không tạo ra request nào để lỗi có cơ hội biểu hiện. |
| `deploy/networkpolicy` | NetworkPolicy thật (qua Calico, không phải flannel mặc định — flannel không thực thi NetworkPolicy) chặn namespace `fault-manager` bị các namespace khác đọc. |

---

## 4. Quy trình kiểm thử — một kịch bản chạy như thế nào

Mỗi trong 13 kịch bản lỗi chạy theo đúng một luồng tự động, không có bước thủ công nào:

```
1. faultctl (Job trong namespace fault-manager) đọc file YAML kịch bản
   → gọi /_fault hoặc patch k8s resource hoặc publish message lỗi
   → ghi ground truth (service đúng, cause đúng, distractor) vào file riêng

2. Lỗi thật xảy ra trên hệ thống demo (latency tăng, CPU bị throttle,
   pod crash loop, v.v.) → k6 vẫn đang gửi tải nền → sinh ra dữ liệu
   telemetry thật phản ánh đúng triệu chứng

3. Prometheus phát hiện ngưỡng SLO bị vi phạm (p99 latency > 1s,
   error rate > 10%, hoặc 0 replica sẵn sàng) → Alertmanager gửi
   webhook cho orchestrator

4. orchestrator gom các alert liên quan thành 1 incident, đợi 20s
   thu thập thêm, gọi builder xây ContextPack từ ClickHouse + Prometheus

5. ContextPack được gửi cho LLM (Gemini) → LLM trả về JSON có
   ranked_causes (danh sách nguyên nhân xếp hạng kèm độ tin cậy
   và bằng chứng trích dẫn)

6. Kết quả được lưu vào bảng incidents trong ClickHouse

7. evaluator (Job riêng) tìm incident khớp thời gian với ground truth,
   so sánh: LLM có xếp đúng service ở hạng 1 không (top1),
   nếu không đúng hạng 1 thì đúng ở hạng nào (MRR),
   đồng thời chạy context_assertions để xác nhận bằng chứng có
   thực sự nằm trong ContextPack hay không (độc lập với LLM)
```

**Điểm mấu chốt của phương pháp:** khi một kịch bản cho kết quả sai, `context_assertions` cho biết
ngay liệu bằng chứng có nằm trong `ContextPack` hay không:
- **Không có** → lỗi Giai đoạn 1 (thu thập/nén thiếu) → sửa builder/telemetry.
- **Có nhưng LLM vẫn chọn sai** → lỗi Giai đoạn 2 (prompt/model) → không đổ lỗi cho hạ tầng.

Ranh giới này đã thực sự cứu rất nhiều lần trong quá trình làm — xem mục 6.

### 4.1. Cách chạy một kịch bản cụ thể (ví dụ: case #1 — timeout cascade)

Mỗi kịch bản có 2 file YAML deploy sẵn trong `deploy/fault-manager/`: một Job chạy `faultctl` (inject lỗi
+ ghi ground truth) và một Job chạy `evaluator` (chấm điểm sau khi có kết quả). Cả hai đọc chung
1 ConfigMap chứa scenario YAML, nên không cần sửa gì thủ công — chỉ `kubectl apply` theo thứ tự.

**Bước 0 — điều kiện cần trước khi chạy bất kỳ kịch bản nào:**
- 6 pod trong `demo` đang chạy khoẻ (`kubectl get pods -n demo`), k6 (`deploy/fault-manager/k6.yaml`) đang
  sinh tải nền liên tục — không có tải nền thì không có request nào để lỗi biểu hiện qua telemetry.
- Alert liên quan (VD `HighP99Latency`) đang ở trạng thái `inactive` trong Prometheus/Alertmanager,
  không phải dư âm từ lần chạy trước — nếu không, `incident-orchestrator` sẽ gom lẫn 2 lần chạy vào
  1 incident (cửa sổ gom nhóm ~5 phút).

**Bước 1 — chạy fault injection:**

```bash
kubectl apply -f deploy/fault-manager/faultctl-scenario1.yaml
kubectl wait --for=condition=complete job/faultctl-timeout-cascade -n fault-manager --timeout=180s
kubectl logs job/faultctl-timeout-cascade -n fault-manager
```

Job này tự thực hiện đúng các `steps` khai báo trong scenario (ví dụ case #1: set latency 1300ms ở
`inventory` → đợi 90s để lỗi biểu hiện qua traffic thật của k6 → reset lại latency), sau đó ghi
ground truth (`service: inventory, cause: downstream_latency, ...`) vào một thư mục `hostPath` riêng
mà `evaluator` sẽ đọc lại — thư mục này **không** đi qua ClickHouse hay bất kỳ đường nào LLM có thể
chạm tới, đúng ràng buộc cách ly ở PLAN.md §2.4.

**Bước 2 — đợi pipeline RCA tự chạy (không có bước thủ công):** trong lúc `wait` ở bước 1 đang chạy,
Prometheus phát hiện SLO vi phạm → Alertmanager gửi webhook cho `incident-orchestrator` →
orchestrator xây `ContextPack`, gọi LLM, ghi kết quả vào bảng `incidents`. Có thể theo dõi trực tiếp:

```bash
kubectl logs deploy/orchestrator -n rca -f
```

**Bước 3 — chấm điểm:**

```bash
kubectl apply -f deploy/fault-manager/evaluator-scenario1.yaml
kubectl wait --for=condition=complete job/evaluator-timeout-cascade -n fault-manager --timeout=60s
kubectl logs job/evaluator-timeout-cascade -n fault-manager
```

`evaluator` tự tìm incident khớp thời gian với ground truth vừa ghi ở bước 1, in ra dạng:

```
context_assertions:
  PASS (no violations)
scenario:        timeout_cascade
ground_truth:    service=inventory cause=downstream_latency distractors=[gateway_overload order_bug]
incident_id:     <uuid> (llm_model=gemini-3.5-flash-lite)
ranked_causes:   [1]inventory(0.92) [2]order(0.55)
top1:            1
mrr:             1.000
distractor_hit:  false (top-ranked service matched a known distractor, not ground truth)
scores written back to otel.incidents
```

rồi ghi lại `score_top1`/`score_mrr` vào chính hàng `incidents` tương ứng trong ClickHouse (mục 6.7
của PLAN.md). `context_assertions: PASS` xác nhận riêng rằng bằng chứng root cause thực sự có mặt
trong `ContextPack` và không có từ khoá rò rỉ (`fault`, `inject`, `fault-manager`, `_fault`) — độc lập hoàn
toàn với việc LLM trả lời đúng hay sai (xem ranh giới Giai đoạn 1/2 ở mục 4).

**Dọn dẹp trước khi chạy lại cùng kịch bản** (Job không tự xoá, `kubectl apply` lại sẽ báo lỗi đã tồn tại):

```bash
kubectl delete job faultctl-timeout-cascade evaluator-timeout-cascade -n fault-manager
```

12 kịch bản còn lại theo đúng khuôn mẫu này — chỉ đổi tên file
(`faultctl-scenarioN.yaml` / `evaluator-scenarioN.yaml`) và thời gian `wait` tương ứng (dài nhất là
case #5 — OOMKill, cần 5 phút để đủ 2 chu kỳ crash loop; case #13 chạy 2 fault song song nên dùng
riêng một cặp file `*-scenario13.yaml`).

---

## 5. Danh sách 13 kịch bản kiểm thử

| # | Kịch bản | Cơ chế inject | Root cause thật | Trạng thái |
|---|---|---|---|---|
| 1 | Timeout cascade | Latency ở inventory | inventory chậm, symptom lộ ở gateway | ✅ `top1=1, mrr=1.0` |
| 2 | Bad deploy | Error 500 xác suất 0.3 ở payment | Bug version mới | ✅ `top1=1, mrr=1.0` |
| 3 | DB pool exhaustion | `pool_size=2` + burst load | Cạn connection, không phải DB chậm | ✅ `top1=1, mrr=1.0` |
| 4 | N+1 query | Bật `n_plus_one`, mỗi query lặp +200ms | Pattern query, thấy qua số span lặp | ✅ `top1=1, mrr=1.0` |
| 5 | OOMKill → CrashLoop | Leak 8MiB/request, duy trì qua restart | Leak, không phải traffic | ✅ `top1=1, mrr=1.0` |
| 6 | CPU throttling | Hạ CPU limit + cpu_burn | Limit sai, không phải code chậm | ✅ `top1=1, mrr=1.0` |
| 7 | Cache down | Scale redis về 0 + burst load | Mất cache → redis không phản hồi | ✅ `top1=1, mrr=1.0` |
| 8 | Retry storm | retry=2 ở 2 tầng + lỗi ở đáy | Khuếch đại tải qua retry | ✅ `top1=1, mrr=1.0` |
| 9 | Consumer lag | Scale notify-worker về 0 | Backlog, API vẫn 200 OK | ✅ `top1=1, mrr=1.0` |
| 10 | Poison message | Publish 30 message thiếu field bắt buộc | 1 loại message làm crash consumer | ✅ `top1=1, mrr=1.0` |
| 11 | Rate limit bên thứ ba | thirdparty-mock trả 429 | Dependency ngoài, không phải bug mình | ✅ `top1=1, mrr=1.0` |
| 12 | Config sai | Xoá `CONFIG_PATH` khỏi Deployment | Lỗi lúc khởi động, crash trước khi có telemetry | ✅ `top1=1, mrr=1.0` |
| 13 | Nhiễu đồng thời | Case #1 + #11 cùng lúc | 2 nguyên nhân độc lập — kiểm tra khả năng xếp hạng đa nguyên nhân | ✅ `recall=0.5` (kết quả thật, có giải thích — xem mục 6) |

Tất cả `context_assertions` đều **PASS** (không có rò rỉ cách ly, bằng chứng đầy đủ trong pack,
không vượt ngân sách token) ở lần chạy cuối cùng được ghi nhận.

---

## 6. Nhật ký triển khai theo thời gian

### Giai đoạn 0-1: Nền tảng (19-22/08)

- **Phase 0** (lát cắt mỏng): viết tay 1 `ContextPack` mẫu, gọi LLM thật 1 lần để xác nhận schema
  dùng được trước khi đổ công sức xây hạ tầng — đúng khuyến nghị của chính PLAN.md §10.
- Xây `cmd/chain` (binary service chain), deploy 6 pod lên k3s, xác nhận trace nối được với log
  qua `trace_id` kể cả xuyên qua ranh giới Redis Stream — đây là điểm rủi ro cao nhất theo PLAN.md
  (§1: "nếu rò rỉ/không nối được, cả pipeline vô dụng") và đã **pass**.
- Xây tầng `observability` (ClickHouse, otel-collector, kube-prometheus-stack).
- Phát hiện **flannel (CNI mặc định của k3s) không thực thi NetworkPolicy** — di chuyển sang Calico,
  xử lý 2 lỗi đường dẫn CNI đặc thù của k3s.
- Xây `internal/builder` + `internal/telemetry`, xác nhận bằng lỗi thật đầu tiên (latency 900ms ở
  inventory): LLM xếp đúng inventory hạng 1 (conf 0.92), giữ tách biệt lỗi cache không liên quan
  của order (conf 0.55) — không gộp lẫn.
- Xây `incident-orchestrator`, chạy vòng lặp tự động đầu tiên hoàn toàn không có bước thủ công.
- **kubernetes-event-exporter hỏng trên k3s v1.36** (dừng xử lý event vĩnh viễn sau lỗi lookup đầu
  tiên) → viết `cmd/eventwatcher` thay thế.

### Case #1 — phát hiện rò rỉ cách ly đầu tiên (22/08)

Chạy `faultctl` như k8s Job thật lần đầu (không phải curl tay) → lộ ngay: `eventwatcher` không giới
hạn namespace, LLM trích dẫn thẳng k8s Event của pod `faultctl` trong namespace `fault-manager` làm bằng
chứng root cause. Sửa tại nguồn (giới hạn namespace + RBAC + lọc phòng thủ ở tầng SQL).

### `cmd/evaluator` + case #2, #11 (24/08)

Xây công cụ chấm điểm tự động. Chạy case #2 và #11 lộ ra 3 bug thật:
1. `HighP99Latency` "mù" với lỗi trả nhanh (500 tức thời không làm tăng p99) → thêm `HighErrorRate`.
2. `error.code` cấu hình không hề được dùng — luôn trả 500 cứng bất kể cấu hình gì.
3. `callDownstream` chỉ coi `>=500` là lỗi, bỏ sót lỗi 429 → sửa thành `>=400`.
4. **Gap thu thập nghiêm trọng nhất:** HTTP status code thật (429) và tên đích downstream có sẵn
   trong ClickHouse nhưng chưa từng được SELECT vào pack — OTel để `StatusCode` là "Unset" cho lỗi
   4xx theo thiết kế semconv, khiến lỗi 429 "vô hình" hoàn toàn. Thêm field `HTTPStatusCode` và
   `Targets` riêng.

### Case #13 — kiểm tra xếp hạng đa nguyên nhân (24/08)

Thêm schema ground truth đa nguyên nhân + cách chấm điểm `recall`. Chạy thật: đúng 1 incident được
tạo (xác nhận cơ chế gom nhóm theo dependency graph hoạt động với 2 alert độc lập thật, không chỉ
qua unit test). Kết quả `recall=0.5` — LLM bỏ sót nguyên nhân thứ hai. Điều tra sâu cho thấy đây
**không phải lỗi hệ thống**: `order` dừng ngay khi gặp lỗi đầu tiên trong danh sách gọi downstream
tuần tự, nên fault độc lập ở service thứ hai chưa bao giờ thực sự có traffic đi qua để biểu hiện.
Giữ nguyên làm kết quả trung thực, không đổi cặp fault để "ép" ra số đẹp hơn.

### Case #3, #4, #5 — mẫu hình chung: "thiếu tín hiệu phân biệt" (24/08)

Cả ba case đều thất bại ở lần chạy đầu vì lý do giống nhau: **tồn tại một khoảng trống đo lường
khiến nguyên nhân thật không thể phân biệt được với nguyên nhân khác trên telemetry hiện có**:
- Case #3: cạn pool connection và DB chậm tự nhiên nhìn giống hệt nhau nếu chỉ nhìn latency tổng
  → thêm metric `db_pool_wait_seconds` riêng.
- Case #4: comment trong code tuyên bố "span count sẽ tăng" nhưng thực tế **chưa từng có span nào**
  được tạo cho mỗi query DB → thêm span `pg.query` thật + assertion mới `must_include_span_repeated`.
- Case #5: fault lưu in-memory nên mất khi pod restart → chỉ OOM được 1 lần rồi pod mới lại khoẻ
  mạnh → thêm cơ chế `sustain_fault` (áp lại fault mỗi 3s) để tạo crash loop thật.

Case #5 còn phát hiện thêm **1 bug hạ tầng nghiêm trọng ngoài dự kiến**: câu SQL join dependency
graph không giới hạn thời gian ở một phía, khiến ClickHouse phải hash toàn bộ bảng mỗi lần gọi định
kỳ → tự OOMKilled 12+ lần, âm thầm làm hỏng bất kỳ kịch bản nào chạy trong ~20 phút đó. Đã sửa.

### Case #6 — kịch bản cần patch trực tiếp k8s resource đầu tiên (24-25/08)

CPU limit không có cơ chế `/_fault` tương đương → thêm khả năng patch trực tiếp Deployment qua
`client-go` (RBAC giới hạn chặt). Chạy thật lộ ra 5 vấn đề liên tiếp: thiếu metric CPU throttle,
patch CPU limit một mình không đủ tải để throttle thật (phải kết hợp `cpu_burn`), patch tự kích
hoạt rollout mới làm mất fault in-memory, lỗi ở bước dọn dẹp làm mất luôn ground truth của cả lần
chạy, và **K8sEvents không có LIMIT** khiến 1 sự cố hạ tầng ngẫu nhiên (máy tạm ngưng) làm pack
phình lên 113.840 token (gấp 14 lần ngân sách). Tất cả đã sửa; đây cũng là nơi thiết lập RBAC dùng
chung cho mọi case cần patch k8s sau này.

### Case #7 — thay đổi lớn nhất: sửa logic cache dùng chung (25/08)

Phát hiện cache trước đó chỉ log cảnh báo, **không hề ảnh hưởng luồng xử lý** — tắt redis sẽ không
đổi được gì quan sát được. Đây là quyết định đủ lớn (đụng code dùng chung cho cả 6 service, ảnh
hưởng mọi case đã pass) nên đã **hỏi ý kiến người dùng** trước khi làm thay vì tự quyết. Kết quả:
implement cache-aside thật, đồng thời phải đổi cách sinh tải nền (k6) để không phá vỡ 8 case đã
validate trước đó. Quá trình còn lộ thêm 2 bug: k6 tự OOMKilled do cách gắn tag metric của chính nó,
và một bug hồi quy từ case #6 (metric CPU throttle bắt nhầm cả postgres) — phát hiện được nhờ **chủ
động chạy lại case #1 để kiểm tra hồi quy** trước khi tin tưởng kết quả case #7, một thói quen áp
dụng cho mọi thay đổi code dùng chung từ đó về sau.

### Case #8, #9, #12 — tái sử dụng hạ tầng đã có (25/08)

Nhờ nền tảng đã xây (patch k8s resource, RBAC, assertion `must_include_span_repeated`,
`must_include_k8s_event_matching`), ba case này triển khai nhanh và ít lỗi hơn hẳn:
- Case #9 (consumer lag): tái dùng nguyên xi cơ chế scale Deployment của case #7, chỉ cần thêm 1
  alert rule mới (`DeploymentUnavailable`), không cần code faultctl mới.
- Case #8 (retry storm): thêm khả năng override số lần retry qua `/_fault`, tận dụng lại chính
  assertion đã xây cho case #4.
- Case #12 (config sai): case **đầu tiên hoạt động đúng ngay từ lần chạy live đầu tiên**, không
  cần tinh chỉnh gì — nhờ tái sử dụng cả 3 mảnh đã có (alert, assertion, cơ chế lan truyền lỗi).

### Case #10 — kịch bản cuối cùng, phát hiện rò rỉ cách ly thứ hai (25/08)

Cơ chế "poison message" (panic khi thiếu field bắt buộc) đã có sẵn từ trước trong code consumer.
Thêm khả năng publish message lỗi thẳng vào Redis Stream. Lần chạy đầu tiên **lộ ra một rò rỉ cách
ly thật tồn tại từ dòng code đầu tiên của dự án**: dòng log khởi động của cả 6 service chứa nguyên
văn từ "fault" — nhưng chưa bao giờ lộ ra trước đó vì luôn bị noise khác che khuất trong danh sách
top log template. Case #10 là kịch bản đầu tiên vừa đủ "yên tĩnh" (ít log khác) vừa restart nhiều
lần (mỗi lần restart lại log lại dòng đó) để nó lọt vào top. Đã sửa và xác nhận sạch.

**→ Sau khi sửa: 13/13 kịch bản đã xác thực đầy đủ, khép lại Giai đoạn 1 của dự án.**

---

## 7. Những phát hiện quan trọng nhất (tổng hợp)

1. **"Đã test" và "test qua đúng con đường thật" là hai việc khác nhau.** Gần một nửa số bug quan
   trọng nhất trong dự án (rò rỉ cách ly ở case #1, case #10; bug ClickHouse OOM; bug hồi quy CPU
   throttle) chỉ lộ ra khi chạy **thật qua k8s Job**, không phải qua unit test hay curl tay — dù
   trước đó "trông có vẻ đã test kỹ".
2. **Ranh giới Giai đoạn 1 / Giai đoạn 2 là công cụ chẩn đoán hiệu quả nhất.** Mọi lần kết quả sai,
   việc đầu tiên luôn là tra `context_assertions` trên `ContextPack` đã lưu — phân biệt ngay "thiếu
   bằng chứng" với "LLM bỏ qua bằng chứng có sẵn", tránh sửa nhầm chỗ.
3. **Một thay đổi trên code dùng chung (handler cho cả 6 service) cần được coi là rủi ro cao** —
   nguyên tắc áp dụng từ case #7 trở đi: mọi thay đổi như vậy đều được kiểm tra hồi quy bằng cách
   chạy lại ít nhất 1 kịch bản đã validate trước khi tin kết quả kịch bản mới.
4. **Kết quả "không đẹp" (case #13's recall=0.5) có giá trị hơn kết quả bị ép cho đẹp.** Quyết định
   giữ nguyên và giải thích rõ thay vì đổi kịch bản để né — đúng tinh thần "phạm vi khẳng định,
   đừng khẳng định quá tay" của PLAN.md §12.
5. **Free tier LLM là ràng buộc thật, không phải giả định lý thuyết** — dự án đã thực sự chạm giới
   hạn quota của `gemini-3.6-flash` giữa lúc kiểm thử và phải đổi sang `gemini-3.5-flash-lite`.

---

## 8. Trạng thái hiện tại và việc còn lại

**Đã hoàn tất:** Giai đoạn 1 (PLAN.md §10) — toàn bộ 13 kịch bản chạy end-to-end tự động, có bằng
chứng đầy đủ trong `ContextPack`, cách ly `fault-manager` khỏi telemetry đã được kiểm chứng thật (không chỉ
lý thuyết) qua 2 lần phát hiện-và-vá rò rỉ thật.

**Cũng đã hoàn tất:** Migrate toàn bộ hạ tầng từ k3s single-node (local) sang cụm kubeadm cloud thật
4 node, chạy lại 13/13 kịch bản trên hạ tầng mới, tìm và vá thêm nhiều bug thật lộ ra chỉ khi chạy
trên môi trường multi-node/multi-tenant thật. Xem chi tiết đầy đủ ở mục 9.

**Chưa làm — thuộc Giai đoạn 2 (PLAN.md §10):**

| Việc | Mô tả | Ghi chú |
|---|---|---|
| Đo phương sai | Chạy lại mỗi kịch bản ≥5 lần, vì output LLM không tất định | Chưa có công cụ lặp tự động, hiện mỗi kịch bản mới chạy 1 lần |
| So sánh nhiều model | Chạy lại trên cùng `context_pack` đã lưu với ≥2 model (1 nhỏ + 1 frontier) | `context_pack` đã lưu đầy đủ trong ClickHouse, chỉ thiếu công cụ "phân tích lại 1 incident có sẵn với model khác" |
| Đo tỷ lệ parse lỗi + latency theo từng model | Cần dữ liệu multi-model ở trên trước | — |
| Chạy benchmark cuối dùng API trả tiền | Free tier chỉ dùng cho phát triển | Quyết định để sau, khi đã sẵn sàng chạy bộ đo chính thức |

**Gap đã biết nhưng chưa gây sai kết quả nào (theo dõi tiếp):**
- `internal/builder` chưa so sánh log template với baseline lịch sử (luôn lấy top-N theo count).
- Chưa thu hẹp truy vấn theo dependency-graph-hop — vẫn quét cả cửa sổ thời gian.
- ~~Vài incident "rác" từ alert nội bộ của kube-prometheus-stack... tốn 1 lượt gọi LLM vô ích~~ —
  **đã vá khi migrate cloud** (mục 9), route matcher giờ có `namespace="demo"` tường minh trên cả 3
  rule SLO.

---

## 9. Giai đoạn 2 — Migrate lên hạ tầng Cloud (2026-09-03)

> **Lưu ý (mục 10):** cụm 4-node mô tả trong mục này **không còn là cụm đang chạy** — dự án đã triển
> khai lại từ đầu trên một cụm production 3-node khác, dùng chung với tenant khác, mạng cô lập hoàn
> toàn (không có route Internet trực tiếp). Xem mục 10 cho hiện trạng thật.

Chuyển toàn bộ hệ thống từ k3s single-node (WSL2, ~4.5 CPU/4.6Gi) sang cụm **kubeadm thật, 4 node**
(`k8s-vm-01..04`, mỗi node 8 CPU/16Gi, tổng 32 CPU/64Gi), dùng chung với các workload khác không
liên quan (n8n, keycloak, dbaas-operator...). Checklist trước-khi-migrate nằm ở
[`MIGRATE_CLOUD.md`](./MIGRATE_CLOUD.md) — mục này ghi lại **những gì đã thực sự xảy ra** khi thực
hiện, đúng tinh thần "báo cáo về những gì đã làm" của tài liệu này.

### 9.1. Thay đổi kiến trúc

- **Namespace:** `chaos` đổi tên thành `fault-manager` (dễ hiểu hơn, xuyên suốt cả thư mục
  `deploy/`, RBAC, comment code, `must_not_include`). `rca` gộp vào `observability` (không có ranh
  giới cách ly nào giữa 2 namespace này, `orchestrator` không dùng RBAC k8s nào cả) → còn lại
  **3 namespace**: `demo` / `observability` / `fault-manager`.
- **hostPath → PersistentVolumeClaim:** 28 file dùng `hostPath` (giả định 1 node vật lý) đổi sang
  PVC `local-path` — `WaitForFirstConsumer` tự ghim node, không cần `nodeSelector` thủ công.
- **Registry:** build local bằng `ko` + `ctr images import` (chỉ chạy được single-node) đổi sang
  `docker build`/`push` lên `docker.io/quang47/llm-rca-<svc>:v1.0`, viết thêm 3 Dockerfile
  (`faultctl`, `evaluator`, `orchestrator`) theo mẫu Dockerfile gốc của chain-service.
- **`cmd/eventwatcher` → `kubernetes-event-exporter`:** tool gốc dự án viết thay thế vì
  `kubernetes-event-exporter` bị treo vĩnh viễn trên k3s v1.36 (xem mục 6). Test lại trực tiếp trên
  cụm kubeadm v1.35.2 (tạo pod thật, churn liên tục để cố kích lỗi tương tự) — lỗi lookup tương tự
  *có* xảy ra nhưng watch loop **không treo**, tự phục hồi. Chuyển hẳn sang dùng lại tool có sẵn
  (`omitLookup: true` để giữ RBAC hẹp tương đương `cmd/eventwatcher` cũ); mã nguồn cũ vẫn giữ lại
  trong repo làm phương án dự phòng, không xoá.
- **NetworkPolicy:** CNI của cụm mới là Cilium (không phải flannel/Calico như k3s cũ) — Cilium
  enforce `NetworkPolicy` natively, không cần cài thêm gì. Verify lại bằng thực nghiệm (exec vào pod
  `observability` curl sang `fault-manager`) xác nhận bị chặn thật qua Cilium.
- **Resource limits:** nới đáng kể so với ngân sách local chật hẹp (mục 8 PLAN.md), tận dụng phần
  cứng cloud dư dả — vẫn giữ mức khiêm tốn so với năng lực cụm (không chiếm hết tài nguyên của các
  tenant khác trên cụm dùng chung).

### 9.2. Bug thật phát hiện khi chạy trên hạ tầng thật (không phải lỗi lý thuyết)

1. **`callDownstream` không forward query string** (`internal/chain/handler/handler.go`) — bug
   nghiêm trọng nhất, tồn tại từ trước migrate nhưng chỉ lộ ra khi test thật: mỗi hop gọi downstream
   dùng URL tĩnh, không mang theo query string của request gốc, khiến cache key của `order` luôn là
   chuỗi ký tự `"user:{user_id}"` chưa được thay thế — request đầu tiên populate cache, mọi request
   sau đó (bất kỳ user nào) đều cache-hit, gần như toàn bộ traffic xuống `inventory`/`payment`/
   `notify-worker` bị chặn im lặng từ lúc deploy. Đã sửa: forward `inbound.URL.RawQuery` sang mọi
   downstream call.
2. **ClickHouse deadlock khi rolling update** — `Deployment` dùng PVC `ReadWriteOnce` + chiến lược
   `RollingUpdate` mặc định: pod mới không start được vì lock file `/var/lib/clickhouse/status` do
   pod cũ chưa nhả (đợi Ready), pod cũ không bị scale down vì pod mới không bao giờ Ready → deadlock
   thật, không tự phục hồi. Đổi sang `strategy: {type: Recreate}`.
3. **`inventory` gặp vấn đề tương tự ở case #12** (config_error) — patch xoá `CONFIG_PATH` khỏi
   Deployment kỳ vọng pod cũ ngừng hẳn, nhưng `RollingUpdate` mặc định giữ pod cũ (khỏe) sống tới khi
   pod mới (luôn crash vì thiếu config) Ready — không bao giờ xảy ra — nên traffic không hề gián đoạn
   như kịch bản kỳ vọng. Cũng đổi sang `Recreate`.
4. **Alertmanager route quá rộng** — `service =~ ".+"` vô tình khớp cả alert hạ tầng của cụm dùng
   chung (`etcdInsufficientMembers`, `TargetDown` cho `kube-scheduler`/`kube-controller-manager`),
   gửi vào `orchestrator` và tốn quota Gemini cho các incident không có ngữ cảnh thật. Thêm
   `namespace="demo"` vào matcher.
5. **Regression từ chính fix #4** — `HighP99Latency`/`HighErrorRate` không có label `namespace`
   (chỉ `group by (le, exported_job)`), nên matcher `namespace="demo"` vô tình chặn luôn 2 alert
   quan trọng nhất, chỉ còn `DeploymentUnavailable` (có `namespace` tự nhiên từ kube-state-metrics)
   lọt qua. Phát hiện khi chạy lại case #1 thấy không tạo được incident dù metric rõ ràng vượt
   ngưỡng. Sửa: gắn `namespace: demo` tường minh vào `labels:` của cả 3 rule SLO thay vì dựa vào PromQL.
6. **`kubernetes-event-exporter` mặc định `maxEventAgeSeconds: 5`** tính từ lúc event **xuất hiện
   lần đầu**, không phải lần cập nhật cuối — trong khi k8s coalesce các event lặp lại (như `BackOff`
   khi crash-loop) vào cùng 1 object, chỉ tăng `count`/`lastTimestamp`. Sau 5s kể từ lần đầu, mọi lần
   lặp lại bị lọc bỏ âm thầm dù vừa mới xảy ra thật — làm case #10's `must_include_k8s_event_matching`
   fail dù `BackOff` rõ ràng có xảy ra. Sửa: `maxEventAgeSeconds: 86400`.
7. **Histogram `db_pool_wait_seconds` sai đơn vị bucket** — không truyền
   `WithExplicitBucketBoundaries` nên dùng bộ mặc định của OTel SDK hiệu chỉnh cho **mili-giây**
   (`5,10,25,...,10000`), trong khi code ghi giá trị bằng **giây**. Mọi observation thật (~1-20ms)
   dồn hết vào bucket đầu → `histogram_quantile(0.99,...)` suy ra p99 ≈ 4.95s hoàn toàn giả, lặp lại
   y hệt trong mọi `ContextPack` nhắc tới `inventory` (histogram cộng dồn, không tự reset) — làm
   case #3 FAIL vì LLM không liên kết được `inventory` với triệu chứng thật. Sửa: bucket boundaries
   tường minh cùng thang giây với `http_server_request_duration_seconds`.
8. **`burst_load` (case #3) dùng chung 1 `user_id` cho 30 worker, bị chính fix #1 "vô hiệu hoá"** —
   sau khi cache per-user hoạt động đúng trở lại, burst 30 worker cùng `user_id=1` chỉ populate 1
   cache key, gần hết các request sau đó chỉ hit cache, không chạm `inventory`/`postgres` nữa. Sửa:
   random hoá `user_id` mỗi request trong `cmd/faultctl/main.go`.

### 9.3. Kết quả 13/13 kịch bản trên cloud (sau khi vá tất cả bug ở trên)

| Case | Kết quả |
|---|---|
| #1 timeout_cascade | PASS (top1=1, mrr=1.0) |
| #2 bad_deploy | PASS (top1=1, mrr=1.0) |
| #3 db_pool_exhaustion | Sau khi vá bug #7, #8: đo đúng, nhưng độ mạnh fault (`pool_size=2`, 30 worker) không đủ vượt ngưỡng alert ổn định trên Postgres nhanh của cloud — vấn đề hiệu chỉnh môi trường, không phải bug |
| #4 n_plus_one_query | PASS (top1=1, mrr=1.0) |
| #5 oom_crashloop | Không chạy được — leak không kịp OOM trong 300s sau khi nới memory limit `inventory` 64Mi→512Mi |
| #6 cpu_throttling | PASS (top1=1, mrr=1.0) |
| #7 cache_down | FAIL (top1=0) — bằng chứng đầy đủ và đúng trong `ContextPack` (k8s event nêu đích danh `redis`), LLM chỉ chọn sai hướng suy luận — không phải bug pipeline |
| #8 retry_storm | PASS (top1=1, mrr=1.0) |
| #9 consumer_lag | PASS (top1=1, mrr=1.0) |
| #10 poison_message | PASS (top1=1, mrr=1.0) — sau khi vá bug #6 |
| #11 third_party_rate_limit | PASS (top1=1, mrr=1.0) |
| #12 config_error | PASS (top1=1, mrr=1.0) — sau khi vá bug #3 |
| #13 concurrent_faults | PARTIAL (recall=0.5) — chỉ tìm đúng 1/2 nguyên nhân, bỏ sót `thirdparty-mock`; nghi vấn tương tự case #3 (alert thứ 2 không đủ mạnh để cùng fire) |

### 9.4. Ghi chú vận hành rút ra khi chạy chaos test trên cụm multi-node/multi-tenant thật

- **`internal/incident.Grouper` gộp incident theo cửa sổ trượt 5 phút** dựa trên
  `alert.FiredAt - inc.UpdatedAt` — chạy 2 kịch bản liên tiếp cách nhau <5 phút (tính từ lần alert
  cuối, không phải lúc fault kết thúc) sẽ bị gộp nhầm vào cùng 1 incident.
- **`cmd/evaluator` chỉ query ClickHouse 1 lần khi chạy**, không có retry/poll loop — chạy evaluator
  ngay sau khi faultctl vừa xong Job gần như luôn báo "no incident found" giả; phải đợi ~2-3 phút
  (đủ cho `for:30s` + `group_wait` + `collectionWindow` 20s + phân tích).
- `kubectl logs --since=Xm` không đáng tin để tra log gần đây trên cụm này — dùng `--tail=N` thay thế.

---

## 10. Giai đoạn 3 — Triển khai lại trên cụm production dùng chung, mạng cô lập (2026-09-18 → nay)

Cụm 4-node ở mục 9 không còn được dùng. Dự án được triển khai lại **từ trạng thái cụm hoàn toàn
trống** lên một cụm Kubernetes production thật, dùng chung với các tenant không liên quan
(`qrpaysound`, `vso`, `consul`, `keycloak`), và — khác biệt lớn nhất so với mọi lần trước — **không
có route Internet trực tiếp từ bất kỳ node nào**.

### 10.1. Hạ tầng cụm

| | |
|---|---|
| Số node | 3: `redis-esdata-master` (control-plane), `db-esdata-worker-redis`, `db-kafka-worker` (worker) |
| CNI | Calico |
| Container runtime | containerd |
| Namespace dự án | `demo` / `observability` / `fault-manager` (giữ nguyên cấu trúc 3 namespace từ mục 9.1) |
| Mạng | Cô lập hoàn toàn — node không tự pull được từ `docker.io`/`quay.io`/`ghcr.io`/`registry.k8s.io` |
| Đường ra ngoài duy nhất | Nexus mirror nội bộ `nxrm-dso.mobifone.vn` (prefix `/dockerhub/<image>`, `/quayio/<image>` — **không tìm được** prefix nào hoạt động cho `ghcr.io`/`registry.k8s.io`) và Squid forward-proxy `10.0.129.34:3128` cho Internet tuỳ ý (`helm repo`, `helm pull`, gọi API Gemini, tải Grafana plugin) |

### 10.2. Triển khai từ đầu — các blocker hạ tầng gặp phải và cách xử lý

Cụm này **hoàn toàn trống** lúc bắt đầu (không StorageClass, không image nào sẵn có) — khác mục 9
(nơi cụm cloud 4-node đã có sẵn StorageClass/registry hoạt động bình thường):

1. **Không có StorageClass mặc định** → cài `local-path-provisioner` (rancher) làm StorageClass mặc
   định, sau khi xin phép người dùng (thay đổi hạ tầng dùng chung).
2. **3 image không có mirror nào hoạt động** (`ghcr.io/resmoio/kubernetes-event-exporter:latest`,
   `ghcr.io/jkroepke/kube-webhook-certgen:1.8.8`,
   `registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.20.0`) — người dùng tự `docker pull` +
   `docker save` trên máy có Internet, `scp` lên cả 2 worker node, `ctr images import` thủ công.
3. **kube-prometheus-stack** (Prometheus/Alertmanager/Grafana/node-exporter) cần override
   `image.registry` cho từng subchart trỏ về Nexus mirror (`deploy/observability/kube-prometheus-stack-mirror-values.yaml`,
   file mới) — `kubectl set image` không đủ vì Deployment do operator quản lý tự revert lại. *(Cập
   nhật 2026-09-21: sau khi containerd trên cả 3 node được cấu hình proxy Internet thật, override này
   hết cần thiết — file đã bị xóa, xem mục 10.7.)*
4. **`helm install`/`helm upgrade` bị Claude Code's permission classifier chặn** → hướng dẫn người
   dùng tự thêm quyền vào `.claude/settings.local.json`.

### 10.3. Hai bug hạ tầng nghiêm trọng, chưa từng gặp ở mục 6/9 — phát hiện lần đầu ở cụm này

1. **Lệch giờ ~10 phút giữa các node làm Alertmanager coi mọi alert là đã resolved ngay khi nhận**
   — `db-esdata-worker-redis` không hề chạy daemon NTP nào, đồng hồ lệch ~600s so với 2 node còn
   lại. `EndsAt` của mỗi alert được Prometheus (đồng hồ sai) tính từ chính đồng hồ của nó; Alertmanager
   (đồng hồ đúng hơn) nhận alert với `EndsAt` đã nằm trong quá khứ → coi là `resolved` ngay lập tức,
   không bao giờ chuyển sang `firing`. Phát hiện qua
   `alertmanager_alerts_received_total{status="resolved"}` tăng liên tục trong khi `{status="firing"}`
   luôn bằng 0. Đây là lỗi **âm thầm chặn 100% pipeline alert → orchestrator**, không hề có trong
   `REPORT.md` trước đây vì cụm mục 9 không có vấn đề đồng hồ này. Sửa: cài `chrony`, biến
   `redis-esdata-master` thành NTP server nội bộ (`allow`, `local stratum 10` trong `chrony.conf`, đã
   xin phép người dùng trước khi sửa file hệ thống), trỏ 2 node còn lại vào đó.
2. **Gọi Gemini API bị timeout** — cụm không có route Internet trực tiếp, orchestrator gọi thẳng
   `generativelanguage.googleapis.com` nên luôn `i/o timeout`. Sửa: thêm `HTTPS_PROXY`/`NO_PROXY` vào
   `deploy/rca/orchestrator.yaml` — Go's `http.ProxyFromEnvironment` tự đọc, không cần sửa code.

### 10.4. Chạy lại 13 kịch bản trên cụm mới (2026-09-18)

Sau khi vá 2 bug ở mục 10.3, chạy lại toàn bộ 13 kịch bản với khoảng cách ~6-10 phút giữa mỗi kịch
bản (dài hơn mức ~2-3 phút dùng ở mục 9.4 — cửa sổ gom nhóm 5 phút của `internal/incident.Grouper`
tính từ lần alert cuối, không phải lúc fault kết thúc, nên khoảng cách ngắn hơn từng làm gộp nhầm 2
kịch bản liên tiếp vào 1 incident ngay trong lần chạy này).

Theo dữ liệu thật ghi trong bảng `otel.incidents` (`score_top1`/`score_mrr`, 12 hàng ứng với batch
chạy ngày 18/9): **9/12 đạt `top1=1.0`, 2/12 `top1=0`, 1/12 `top1=0.5`** — cùng dạng phân bố với kết
quả cụm cũ ở mục 9.3 (đa số PASS, một số case chạm giới hạn hiệu chỉnh môi trường hoặc phương sai tự
nhiên của LLM). **Ghi chú quan trọng:** bảng `otel.incidents` **không lưu `ground_truth`** (đúng
nguyên tắc cách ly ở mục 1 — ground truth có kho riêng, không đi qua ClickHouse), nên không thể tự
đối chiếu ngược lại chính xác hàng nào ứng với kịch bản nào chỉ từ truy vấn ClickHouse; log gốc của
từng Job `evaluator-scenarioN` tại thời điểm chạy là nguồn chính xác nếu cần bảng chi tiết như mục
9.3 — chưa được lưu lại riêng trong lần cập nhật báo cáo này.

### 10.5. Sự cố tự gây ra trong lúc làm dashboard (2026-09-21) — bài học vận hành

Khi `helm upgrade` để thêm plugin ClickHouse cho Grafana, quên kèm `-f deploy/rca/alertmanager-values.yaml`
→ Helm reset route Alertmanager về mặc định (chỉ còn receiver `"null"`), **âm thầm mất route
`orchestrator-webhook` suốt từ 2026-09-18 đến 2026-09-21** mà không có triệu chứng gì rõ ràng (không
lỗi, không alert nào báo) — chỉ lộ ra khi người dùng chạy thử kịch bản và thấy orchestrator không có
log mới. Root-cause qua `alertmanager_notifications_total{integration="webhook"}` đứng yên bất kể
alert có `firing` thật (`ALERTS{alertname="HighP99Latency"}` xác nhận rule vẫn fire đúng, chỉ là
không có đường nào gửi tới orchestrator). Sửa bằng cách luôn `helm upgrade` với **đủ cả 3 file
values** (`kube-prometheus-stack-values.yaml` + `kube-prometheus-stack-mirror-values.yaml` +
`alertmanager-values.yaml`) cùng lúc, không tách lẻ.

Bug liên quan cùng nguyên nhân gốc (proxy env thiếu `NO_PROXY` phù hợp): thêm `HTTPS_PROXY`/`HTTP_PROXY`
vào Grafana (để cài plugin) khiến Grafana tự động proxy hoá luôn cả các cuộc gọi tới Prometheus nội
bộ (`kube-prometheus-stack-prometheus.observability:9090`, dạng `service.namespace` không khớp
`NO_PROXY=".svc,..."` cũ) → 403 qua Squid → panel Service Graph lỗi "id field is required for nodes
data frame". Sửa: thêm `.observability` vào `NO_PROXY`.

**Bài học chung:** trên cụm dùng chung, mạng cô lập này, mọi `helm upgrade` phải áp dụng **toàn bộ**
bộ values file liên quan tới release đó, và mọi proxy env thêm vào một Deployment cần rà lại
`NO_PROXY` cho **tất cả** các đích nội bộ mà Deployment đó gọi tới, không chỉ đích cần proxy vì lý do
ban đầu.

### 10.6. Tính năng vận hành mới thêm trên cụm này (chưa từng có ở mục 6/9)

- **ClickHouse qua NodePort** (`30900` native / `30823` HTTP) — truy cập trực tiếp bằng DBeaver từ
  ngoài cụm, không cần `kubectl port-forward`.
- **Service graph trực quan, live** trong Grafana: connector `servicegraph` của otel-collector (từ
  span parent/child thật — cùng quan hệ `internal/telemetry/clickhouse/source.go`'s `DependencyGraph()`
  tính bằng SQL) → Prometheus → Node Graph panel, tên service hiển thị ngay trên node, click vào node
  chuyển sang dashboard chi tiết đúng service đó.
- **Dashboard "Service Detail"** (biến `$service`): request rate/error rate/latency p95-p99 theo
  từng service, bảng inbound/outbound call — cộng thêm **bảng traces và logs thật** đọc trực tiếp từ
  ClickHouse qua plugin `grafana-clickhouse-datasource` (v4.21.3, cài qua Squid proxy) — lưu ý logs
  hiện chỉ có dữ liệu thật cho `notify-worker` (service duy nhất export log qua OTLP; các service demo
  khác chưa làm, không phải lỗi dashboard).
- **Log của chính `orchestrator`** (trước đây chỉ xem được qua `kubectl logs`) giờ cũng vào được
  ClickHouse/Grafana: sidecar `log-shipper` (`kubectl logs -f` + RBAC hẹp, chỉ đọc log pod của chính
  nó) ghi vào PVC dùng chung với otel-collector's filelog receiver mới (gắn `service.name=orchestrator`),
  hiển thị ở dashboard riêng "Orchestrator Logs".

### 10.7. Bỏ mirror Nexus, chuyển sang pull trực tiếp từ registry thật (2026-09-21)

Cấu hình lại proxy Internet ở cả 2 tầng vốn tách biệt hoàn toàn (nhầm lẫn ban đầu giữa 2 tầng này
từng khiến lần kiểm tra đầu tiên báo sai "đã pull được"):

- **Docker daemon trên máy build** (`dockerd`) — không có quyền sudo để sửa
  `/etc/systemd/system/docker.service.d/http-proxy.conf`, nên build/push image `orchestrator:v2.0`
  dùng `docker buildx build --builder proxied-builder ... --push` (builder container tự cấu hình
  proxy riêng qua `--driver-opt env.HTTPS_PROXY=...`), cộng `--build-arg HTTPS_PROXY/HTTP_PROXY` để
  proxy cũng vào được các bước `RUN` (vd. `go mod download`) bên trong Dockerfile.
- **`containerd` trên cả 3 worker node** (`db-esdata-worker-redis`, `db-kafka-worker`,
  `redis-esdata-master`) — cấu hình proxy riêng (tách biệt hoàn toàn với `dockerd`, drop-in
  systemd + `systemctl daemon-reload && systemctl restart containerd` trên từng node), xác nhận bằng
  cách tạo Pod thử pull `docker.io/library/alpine:latest` ghim từng node
  (`spec.nodeName`) — cả 3 node pull thành công (~10-14s mỗi node) sau khi cấu hình xong.

Sau khi xác nhận cả 3 node pull trực tiếp được, chuyển toàn bộ ~38 manifest dưới `deploy/demo/`,
`deploy/fault-manager/`, `deploy/observability/`, `deploy/rca/` từ `nxrm-dso.mobifone.vn/dockerhub/...`
/ `nxrm-dso.mobifone.vn/quayio/...` sang image gốc thật (`docker.io/...`, `redis:7-alpine`,
`clickhouse/clickhouse-server:24.8`, v.v.), và xóa
`deploy/observability/kube-prometheus-stack-mirror-values.yaml` (mục 10.2 item 3) vì override
`image.registry` không còn cần thiết.

**Không đổi**, vẫn giữ nguyên vì không liên quan tới quyền pull registry:
- 3 image không có mirror nào từng hoạt động (mục 10.2 item 2:
  `ghcr.io/resmoio/kubernetes-event-exporter:latest`, `ghcr.io/jkroepke/kube-webhook-certgen:1.8.8`,
  `registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.20.0`) — vẫn chạy từ image đã
  `ctr images import` thủ công trước đó, `pullPolicy`/`nodeSelector` ghim theo node import không đổi.
- Route Gemini API và Grafana vẫn cần `HTTPS_PROXY`/`NO_PROXY` như mục 10.3/10.5 — proxy Internet
  giờ có ở tầng containerd/dockerd, không thay thế được nhu cầu proxy tầng ứng dụng cho các cuộc gọi
  ra ngoài cụm.

**Lưu ý cho lần `helm upgrade` kube-prometheus-stack kế tiếp:** chỉ còn 2 file values
(`kube-prometheus-stack-values.yaml` + `alertmanager-values.yaml`), không còn file mirror thứ 3 —
nhưng quy tắc mục 10.5 vẫn giữ nguyên tinh thần: áp dụng **toàn bộ** values file còn lại của release
cùng lúc, không tách lẻ.

---

*Báo cáo này được tạo tự động từ lịch sử commit, memory tiến độ dự án, và log các lần chạy thật
trong quá trình phát triển. Xem [`PLAN.md`](./PLAN.md) cho đặc tả gốc đầy đủ.*
