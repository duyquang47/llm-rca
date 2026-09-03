# Checklist migrate llm-rca lên k8s cloud

Tài liệu này ghi lại toàn bộ điểm cần xử lý khi chuyển dự án từ k3s single-node (WSL2, 8-12
CPU/16GB, xem [`PLAN.md`](./PLAN.md) mục 1) sang một cụm Kubernetes trên cloud. Dùng làm checklist
khi làm việc, và mang theo phiên trò chuyện tiếp theo để tôi (Claude) kiểm tra lại kết quả.

**Trạng thái khi viết file này:** repo đang ở trạng thái 13/13 kịch bản đã xác thực trên hạ tầng
local (xem [`BAO_CAO_DU_AN.md`](./BAO_CAO_DU_AN.md)). Chưa có bất kỳ thay đổi nào cho migrate được
áp dụng — toàn bộ mục dưới đây là **việc cần làm**, không phải đã làm.

---

## 0. Trước khi push code lên git

- [ ] Repo hiện **chưa có remote nào** (`git remote -v` rỗng) — cần `git remote add origin <url>`
      trước khi push.
- [ ] Xác nhận không có secret nào bị commit nhầm: đã kiểm tra `GEMINI_API_KEY` chỉ xuất hiện qua
      `os.Getenv`/`secretKeyRef` (`deploy/rca/orchestrator.yaml`), không hardcode ở đâu — nhưng nên
      tự `git grep -i "AIza\|api_key.*=.*['\"]"` lại lần nữa trước khi push lần đầu lên remote mới.
- [ ] `CLAUDE.md` đã được đổi tên thành [`PLAN.md`](./PLAN.md) trong phiên làm việc gần đây — nếu
      có tool/script nào ở phía cloud tự đọc đúng tên `CLAUDE.md`, cần cập nhật lại tên file.

---

## 1. BLOCKER — `hostPath` giả định mọi pod cùng 1 node vật lý

Có **28 file YAML** dùng `hostPath`, thuộc 2 cặp ghi/đọc chéo nhau:

| Ghi | Đọc | Path dùng chung |
|---|---|---|
| `deploy/observability/eventwatcher.yaml` | `deploy/observability/otel-collector.yaml` | `/var/log/k8s-events` |
| `deploy/fault-manager/faultctl-scenarioN.yaml` (×13) | `deploy/fault-manager/evaluator-scenarioN.yaml` (×13) | `/var/lib/faultctl/ground-truth` |

Trên k3s single-node, "ghi" và "đọc" luôn chạm cùng 1 ổ đĩa vật lý nên hoạt động đúng. Trên cluster
nhiều node thật, 2 pod này có thể bị scheduler đặt ở 2 node khác nhau → bên đọc thấy thư mục rỗng,
**không báo lỗi gì**, chỉ âm thầm mất k8s events hoặc mất ground truth của cả lần chạy.

**Cách xử lý tuỳ loại cụm:**

- [ ] **Nếu cụm cloud vẫn là 1 node** (VM to hơn, tự cài k3s/k8s) → không cần sửa gì, giữ nguyên
      `hostPath`. Đây là hướng ít việc nhất cho một POC.
- [ ] **Nếu cụm nhiều node thật** (EKS/GKE/AKS managed, hoặc self-managed nhiều node) → phải chọn 1
      trong 2:
  - Đổi `hostPath` → `PersistentVolumeClaim` (ReadWriteOnce) + ghim 2 pod liên quan vào cùng node
    bằng `nodeSelector` hoặc pod affinity (sửa ít, không đổi kiến trúc).
  - Đổi sang PVC `ReadWriteMany` trên storage class hỗ trợ (EFS/Filestore/Azure Files) để pod tự do
    nằm bất kỳ node nào (sửa nhiều hơn, "đúng chuẩn cloud" hơn).
- [ ] `deploy/observability/clickhouse.yaml` đã dùng `persistentVolumeClaim` sẵn (không phải
      `hostPath`) — không cần đụng vào, chỉ cần cụm cloud có StorageClass mặc định hoạt động.

---

## 2. BLOCKER — image build/push không qua registry

Hiện tại **5 image** (`chain-service:1.0`, `faultctl:1.0`, `evaluator:1.0`, `orchestrator:1.0`,
`eventwatcher:1.0`) được build bằng `ko` rồi **import thủ công** vào containerd của node local qua
`sudo k3s ctr images import` — không hề đi qua registry nào. Cách này không chạy được trên cluster
cloud (không có quyền `ctr` trực tiếp vào node của managed cluster, và nhiều node thì import 1 node
cũng vô nghĩa).

- [ ] Chọn 1 container registry (ECR/GCR/Artifact Registry/ACR nếu dùng registry của chính cloud
      provider — thường nhanh nhất vì cùng hạ tầng; hoặc Docker Hub/ghcr.io nếu muốn đơn giản setup).
- [ ] Set `KO_DOCKER_REPO=<registry>/<path>` rồi build bằng `ko build --push` (hoặc tương đương)
      thay vì build local + import tay.
- [ ] Đổi tên image trong **toàn bộ YAML** từ dạng trần (`chain-service:1.0`) sang
      `<registry>/<path>/chain-service:1.0` — cùng tên lặp lại ở nhiều file, nên dùng `sed` hàng
      loạt thay vì sửa tay từng file. Danh sách file bị ảnh hưởng: mọi file dưới `deploy/demo/`,
      `deploy/fault-manager/faultctl-scenario*.yaml`, `deploy/fault-manager/evaluator-scenario*.yaml`,
      `deploy/rca/orchestrator.yaml`, `deploy/observability/eventwatcher.yaml`.
- [ ] Đổi `imagePullPolicy: IfNotPresent` → `Always` ở toàn bộ các file trên (hiện tại đang giả định
      image luôn đã có sẵn local, không đúng nữa khi pull thật từ registry).
- [ ] Nếu registry private → thêm `imagePullSecrets` vào ServiceAccount/pod spec liên quan.

---

## 3. CẦN KIỂM CHỨNG LẠI — NetworkPolicy có thực sự chặn không

Dự án từng phát hiện **flannel (CNI mặc định k3s) không thực thi NetworkPolicy** — phải chuyển sang
Calico mới chặn thật được namespace `fault-manager` (xem `BAO_CAO_DU_AN.md` mục 6, "Giai đoạn 0-1"). Đây là
phát hiện từ live-test thật, không phải giả định lý thuyết — CNI mặc định của managed k8s **không
chắc** đã thực thi NetworkPolicy:

- EKS: CNI mặc định (`aws-vpc-cni`) không thực thi NetworkPolicy trừ khi cài thêm Calico hoặc bật
  add-on riêng.
- GKE: cần bật `--enable-network-policy` lúc tạo cluster, không mặc định.
- AKS: cần chọn `--network-policy calico` (hoặc `azure`) lúc tạo cluster.

- [ ] Xác nhận CNI của cụm cloud có thực thi NetworkPolicy — nếu không, cài Calico (hoặc bật add-on
      tương ứng) trước khi tin `deploy/networkpolicy` có tác dụng gì.
- [ ] **Test lại bằng chính cách dự án đã làm trước đây**: thử truy cập chéo từ pod ở `demo`/`rca`
      sang namespace `fault-manager`, xác nhận bị **chặn thật** (connection refused/timeout), đừng chỉ tin
      `kubectl apply` chạy không báo lỗi — `apply` luôn thành công dù CNI không thực thi gì cả.

---

## 4. NÊN LÀM — nới resource limits (đúng mục đích chính của việc migrate)

Toàn bộ `requests`/`limits` hiện tại được tinh chỉnh khít trong ~4.5 CPU/4.6Gi RAM (giới hạn phần
cứng local ghi ở `PLAN.md` mục 1). Đây là 2 việc đã ghi nhận nhưng **cố ý hoãn lại** trong bản đánh
giá tính khả thi trước đây (đợi lúc migrate cloud mới xử lý):

- [ ] Nới CPU/memory cho ClickHouse — đây từng là điểm nghẽn nặng nhất (bug join `DependencyGraph`
      từng OOM ở giới hạn 1Gi khi bảng chỉ mới 86MiB, xem `internal/telemetry/clickhouse/source.go`
      comment ở hàm `DependencyGraph`).
- [ ] Cân nhắc tăng `collectionWindow` (hiện 20s, ở `cmd/orchestrator/main.go`) hoặc tần suất scrape
      Prometheus nếu muốn context pack chi tiết hơn giờ có tài nguyên dư dả.
- [ ] Cân nhắc tăng TTL ClickHouse (hiện 48h, ở `deploy/observability/otel-collector.yaml`) nếu
      muốn giữ dữ liệu lâu hơn phục vụ Giai đoạn 2 (so sánh nhiều model trên cùng context_pack đã
      lưu — xem `BAO_CAO_DU_AN.md` mục 8).

---

## 5. Việc phụ, làm 1 lần khi có cụm mới

- [ ] Tạo lại Secret `llm-api-keys` (chứa `gemini-api-key`) thủ công bằng `kubectl create secret` —
      secret này **không** nằm trong git (đúng chủ đích), nên đây là bước dễ quên nhất khi dựng lại
      cụm mới từ đầu.
- [ ] `WATCH_NAMESPACE=demo` của `cmd/eventwatcher` và cấu trúc 4 namespace (`demo`/`observability`/
      `rca`/`fault-manager`) không phụ thuộc gì vào hạ tầng cloud/local — giữ nguyên, không cần sửa.
- [ ] RBAC (`ClusterRole`/`ClusterRoleBinding` cho `otel-collector`, `eventwatcher`, `faultctl`)
      cũng không phụ thuộc cloud/local — chỉ cần đảm bảo user apply YAML có đủ quyền tạo RBAC trên
      cụm mới (một số tổ chức khoá quyền tạo `ClusterRole` cho user thường).

---

## 6. Sau khi migrate xong — cách kiểm chứng lại (nhờ Claude check)

Không tin "deploy xong không lỗi" là đủ — dự án này có tiền lệ nhiều bug chỉ lộ ra khi **chạy thật
qua k8s Job** (xem `BAO_CAO_DU_AN.md` mục 7, phát hiện #1: "đã test" và "test qua đúng con đường
thật" là 2 việc khác nhau). Khi quay lại nhờ tôi kiểm tra, nên chạy tối thiểu:

1. Chạy lại case #1 (`timeout_cascade`, đơn giản nhất, đã có sẵn `deploy/fault-manager/faultctl-scenario1.yaml`
   + `evaluator-scenario1.yaml`) — xác nhận `top1=1, mrr=1.0` và `context_assertions: PASS` giống hệt
   kết quả local trước đây.
2. Chạy 1 case có patch k8s resource trực tiếp (case #6 CPU throttling, hoặc #12 config error) — đây
   là nhóm dễ vỡ nhất nếu RBAC/quyền trên cụm mới thiếu.
3. Chạy case #10 (poison message) hoặc case #7 (cache down) — 2 case từng phát hiện rò rỉ cách ly
   thật (`must_not_include` fail) — chạy lại để xác nhận cách ly vẫn giữ được trên cụm mới, không
   chỉ tin log cũ từ local.
4. Nếu chuyển sang multi-node: cố tình xoá pod `eventwatcher` hoặc `faultctl` để buộc scheduler xếp
   lại vị trí, rồi chạy lại 1 case — đây là cách trực tiếp nhất để phát hiện mục 1 (hostPath) có vỡ
   hay không mà không cần đợi may rủi.

---

## 7. Trạng thái phiên làm việc gần nhất (2026-09-03) — ĐỌC TRƯỚC KHI TIẾP TỤC

**Migrate đã xong và đã verify.** Cụm đích: kubeadm thật, 4 node (`k8s-vm-01..04`, 8 CPU/16Gi mỗi
node), namespace `demo`/`observability`/`fault-manager` (đổi từ `chaos`, gộp `rca` vào
`observability`). Registry: `docker.io/quang47/llm-rca-<svc>:v1.0`. Chi tiết đầy đủ nằm trong các
commit log (xem `git log --oneline`, 12 commit gần nhất từ `4bc1e73` "Migrate deployment to real
kubeadm cloud cluster" trở đi), không lặp lại ở đây.

**13/13 kịch bản đã chạy trên cloud** (lần chạy đầu, trước khi đào sâu case #3 — xem cập nhật ngay
dưới): 9 PASS trọn vẹn (#1, #2, #4, #6, #8, #9, #10, #11, #12), 2 FAIL (#3, #7), 1 PARTIAL (#13,
recall=0.5), 1 không chạy được (#5, do nới resource limit).

### Case #3 (db_pool_exhaustion) — đã đào sâu, tìm ra **2 bug thật**, đã sửa cả hai

**Bug 1 — histogram `db_pool_wait_seconds` sai đơn vị bucket** (đã fix, commit `a1a4c99`): định
nghĩa ở `internal/chain/handler/handler.go` hàm `dbPoolWait` không truyền
`WithExplicitBucketBoundaries` nên OTel SDK dùng bộ mặc định `{5,10,25,...,10000}` (hiệu chỉnh cho
giá trị **mili-giây**), trong khi code ghi giá trị bằng **giây** (`time.Since(acquireStart).Seconds()`,
pool wait thật ~0.001-0.02s). Mọi observation thật dồn vào 1 bucket đầu → `histogram_quantile(0.99,...)`
suy ra p99 ≈ 4.95s hoàn toàn giả, xuất hiện lặp lại y hệt trong **mọi** ContextPack nhắc tới
`inventory` (kể cả context pack của case #7, không liên quan) vì đây là histogram cộng dồn. Đã sửa
bằng bucket boundaries tường minh cùng thang giây với `http_server_request_duration_seconds`
(`0.001` tới `10`). **Đã verify runtime sau redeploy**: `db_pool_wait_seconds_p99` giờ đúng ~0.001s ở
trạng thái bình thường (trước đây báo giả 4.95 cố định). *(Lưu ý: lúc kiểm tra ngay sau redeploy —
~20s — vẫn thấy bucket cũ do chưa đủ sample mới tích luỹ qua 1 chu kỳ scrape của Prometheus; đợi vài
phút rồi kiểm tra lại là thấy đúng — không phải bug thật, chỉ là kiểm tra quá sớm.)*

**Bug 2 — `burst_load` dùng chung 1 `user_id` cho cả 30 worker, bị cache của chính fix trước "vô
hiệu hoá"** (đã fix, commit `efec92a`): sau khi fix bug forward query string (`3dfbb20`), cache
per-user_id của `order` hoạt động đúng trở lại — nhưng case #3's `burst_load` hard-code
`user_id=1` cho toàn bộ 30 worker đồng thời, nên sau request đầu tiên populate cache, gần như mọi
request sau đó (kể cả từ worker khác) chỉ hit cache và **không** chạm tới `inventory`/`postgres`
nữa — vô tình làm mất tác dụng của cả bài test pool exhaustion. Đã sửa `cmd/faultctl/main.go` hàm
`burstLoad`: random hoá `user_id` mỗi request để né cache, đúng tinh thần "chạm thật vào DB pool".
**Đã verify runtime**: sau fix, `inventory`'s p99 riêng và `db_pool_wait_seconds` đều tăng thật rõ
trong lúc fault (trước đó phẳng lì ở baseline).

**Còn lại — không phải bug, là giới hạn hiệu năng môi trường:** dù cả 2 fix trên đã đúng và đã
verify, độ nghiêm trọng tuyệt đối của fault (`pool_size=2`, 30 worker, Postgres cùng node rất nhanh
trên cụm cloud này) vẫn **không đủ để vượt ngưỡng `HighP99Latency` (p99 > 1s)** một cách ổn định —
gateway p99 quan sát được dao động 0.45-1.76s tuỳ lần chạy, không đủ chắc chắn vượt ngưỡng trong
`for: 30s`. Không tạo được alert → không có incident → evaluator không chấm điểm được. Đây là vấn đề
**hiệu chỉnh độ mạnh của fault cho phần cứng cloud nhanh hơn**, không phải bug code — cần quyết định
của người dùng nếu muốn tiếp tục: (a) hạ `pool_size` xuống 1, (b) tăng `concurrency`/`duration` trong
`scenarios/03_db_pool_exhaustion.yaml`, hoặc (c) thêm độ trễ giả lập cho mỗi query khi pool đang cạn
(giống cách case #4 n_plus_one_query dùng `roundTripCost` giả lập chi phí network thật), hoặc (d)
chấp nhận case #3 "flaky trên môi trường nhanh" như một phát hiện, không sửa thêm.

### Case #7, #5, #13 — đã điều tra, kết luận

- **Case #7 (cache_down) FAIL không phải bug** — đã kiểm tra ContextPack, bằng chứng đầy đủ và đúng
  (k8s event `ScalingReplicaSet` nêu đích danh `redis`, log lỗi cache của `order`), LLM chỉ chọn sai
  hướng suy luận (attribute cho service có log lỗi thay vì dependency thực sự hỏng). Không sửa thêm.
- **Case #5 (oom_crashloop)** không chạy được vì đã nới memory limit `inventory` 64Mi→512Mi (theo
  yêu cầu nới resource limit) khiến leak không kịp OOM trong 300s. Muốn test lại: (a) tăng
  `sustain_fault` duration trong `scenarios/05_oom_crashloop.yaml`, hoặc (b) chấp nhận đánh đổi đã
  biết, không sửa.
- **Case #13 (concurrent_faults)** recall=0.5 — context_assertions PASS (bằng chứng `thirdparty-mock`
  có mặt trong pack) nhưng LLM chỉ rank được `inventory`, bỏ sót `thirdparty-mock`. Chưa điều tra sâu
  — nghi vấn liên quan cách builder chọn candidate services khi chỉ 1 trong 2 alert thực sự fire
  (xem log lúc chạy: incident chỉ có `services: ["gateway","inventory","order"]`, KHÔNG có
  `thirdparty-mock`/`payment` — có thể `thirdparty-mock`'s alert riêng không đủ mạnh để fire cùng
  lúc, y hệt kiểu vấn đề case #3 vừa gặp). Đáng xem lại nếu muốn đào tiếp.

### Ghi chú vận hành quan trọng, rút ra từ phiên này (áp dụng khi chạy case nào khác)

- **`internal/incident.Grouper` gộp incident theo cửa sổ trượt 5 phút** (`defaultWindow` trong
  `internal/incident/grouper.go`) dựa trên `alert.FiredAt - inc.UpdatedAt`, không phải theo scenario.
  Chạy 2 case liên tiếp cách nhau <5 phút (tính từ **lần alert cuối cùng**, không phải lúc fault kết
  thúc) sẽ bị gộp vào cùng 1 incident, kết quả evaluator sẽ sai/lẫn lộn. Luôn kiểm tra
  `curl alertmanager:9093/api/v2/alerts?filter=namespace="demo"` trả về rỗng VÀ đã rỗng ổn định vài
  phút trước khi chạy case tiếp theo — đừng chỉ đợi một khoảng thời gian cố định.
- **`cmd/evaluator` chỉ query ClickHouse 1 lần duy nhất khi chạy** (`findIncident` trong
  `cmd/evaluator/main.go`, không có retry/poll loop dù tham số `-search-window` mặc định 10 phút chỉ
  là biên tìm kiếm, không phải thời gian chờ). Áp dụng evaluator ngay sau khi faultctl vừa xong Job
  gần như luôn báo "no incident found" giả — phải đợi ~2-3 phút (đủ cho alert `for:30s` +
  `group_wait` + `collectionWindow` 20s + phân tích) rồi mới `kubectl apply` evaluator.
- **`kubectl logs --since=Xm` không đáng tin** để tra log gần đây — dùng `--tail=N` thay thế (đã gặp
  trường hợp `--since` bỏ sót log thật có tồn tại).
- Deployment dùng PVC RWO + `RollingUpdate` mặc định có thể **deadlock thật** nếu pod mới không bao
  giờ Ready (ClickHouse) hoặc cố ý fail (case #12 config_error trên `inventory`) — cả 2 đã đổi sang
  `strategy: {type: Recreate}`.

---

*File này được tạo theo yêu cầu trước khi migrate dự án sang k8s cloud — tổng hợp từ khảo sát trực
tiếp các file `deploy/` hiện có (28 file dùng `hostPath`, 5 tên image build bằng `ko`) và các bài
học thật đã ghi trong `BAO_CAO_DU_AN.md`.*
