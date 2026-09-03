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

*File này được tạo theo yêu cầu trước khi migrate dự án sang k8s cloud — tổng hợp từ khảo sát trực
tiếp các file `deploy/` hiện có (28 file dùng `hostPath`, 5 tên image build bằng `ko`) và các bài
học thật đã ghi trong `BAO_CAO_DU_AN.md`.*
