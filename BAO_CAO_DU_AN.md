# Báo cáo dự án: LLM-based RCA cho hệ thống microservices

**Thời gian thực hiện:** 2026-08-19 → 2026-08-25 (7 ngày, 44 commit)
**Trạng thái:** 13/13 kịch bản lỗi đã được xác thực end-to-end (Giai đoạn 1 hoàn tất theo CLAUDE.md §10)

---

## 1. Mục tiêu dự án

Xây dựng một hệ thống dùng LLM để tóm tắt log/trace/alert và gợi ý nguyên nhân gốc (root cause)
cho sự cố trên các dịch vụ chạy trong Kubernetes — sau đó **đo đạc định lượng** độ chính xác của
gợi ý đó, thay vì chỉ đánh giá cảm tính "câu trả lời có vẻ hợp lý".

Đây là **POC nghiên cứu**, không phải sản phẩm. Tiêu chí thành công: top-1 accuracy và MRR
(Mean Reciprocal Rank) trên bộ 13 kịch bản lỗi có ground truth biết trước.

Toàn bộ đặc tả gốc nằm trong [`CLAUDE.md`](./CLAUDE.md) — tài liệu này là báo cáo về **những gì
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
chạm tới, đúng ràng buộc cách ly ở CLAUDE.md §2.4.

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
của CLAUDE.md). `context_assertions: PASS` xác nhận riêng rằng bằng chứng root cause thực sự có mặt
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
  dùng được trước khi đổ công sức xây hạ tầng — đúng khuyến nghị của chính CLAUDE.md §10.
- Xây `cmd/chain` (binary service chain), deploy 6 pod lên k3s, xác nhận trace nối được với log
  qua `trace_id` kể cả xuyên qua ranh giới Redis Stream — đây là điểm rủi ro cao nhất theo CLAUDE.md
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
   đừng khẳng định quá tay" của CLAUDE.md §12.
5. **Free tier LLM là ràng buộc thật, không phải giả định lý thuyết** — dự án đã thực sự chạm giới
   hạn quota của `gemini-3.6-flash` giữa lúc kiểm thử và phải đổi sang `gemini-3.5-flash-lite`.

---

## 8. Trạng thái hiện tại và việc còn lại

**Đã hoàn tất:** Giai đoạn 1 (CLAUDE.md §10) — toàn bộ 13 kịch bản chạy end-to-end tự động, có bằng
chứng đầy đủ trong `ContextPack`, cách ly `fault-manager` khỏi telemetry đã được kiểm chứng thật (không chỉ
lý thuyết) qua 2 lần phát hiện-và-vá rò rỉ thật.

**Chưa làm — thuộc Giai đoạn 2 (CLAUDE.md §10):**

| Việc | Mô tả | Ghi chú |
|---|---|---|
| Đo phương sai | Chạy lại mỗi kịch bản ≥5 lần, vì output LLM không tất định | Chưa có công cụ lặp tự động, hiện mỗi kịch bản mới chạy 1 lần |
| So sánh nhiều model | Chạy lại trên cùng `context_pack` đã lưu với ≥2 model (1 nhỏ + 1 frontier) | `context_pack` đã lưu đầy đủ trong ClickHouse, chỉ thiếu công cụ "phân tích lại 1 incident có sẵn với model khác" |
| Đo tỷ lệ parse lỗi + latency theo từng model | Cần dữ liệu multi-model ở trên trước | — |
| Chạy benchmark cuối dùng API trả tiền | Free tier chỉ dùng cho phát triển | Quyết định để sau, khi đã sẵn sàng chạy bộ đo chính thức |

**Gap đã biết nhưng chưa gây sai kết quả nào (theo dõi tiếp):**
- `internal/builder` chưa so sánh log template với baseline lịch sử (luôn lấy top-N theo count).
- Chưa thu hẹp truy vấn theo dependency-graph-hop — vẫn quét cả cửa sổ thời gian.
- Vài incident "rác" từ alert nội bộ của kube-prometheus-stack (không liên quan tới namespace
  `demo`) thỉnh thoảng lọt qua route matcher, tốn 1 lượt gọi LLM vô ích — chưa lọc.

---

*Báo cáo này được tạo tự động từ lịch sử commit (44 commit), memory tiến độ dự án, và log các lần
chạy thật trong quá trình phát triển. Xem [`CLAUDE.md`](./CLAUDE.md) cho đặc tả gốc đầy đủ.*
