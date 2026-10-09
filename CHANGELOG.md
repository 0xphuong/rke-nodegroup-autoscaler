# Changelog

Mọi thay đổi đáng chú ý của dự án được ghi ở đây. Định dạng theo [Keep a Changelog](https://keepachangelog.com/),
version theo [SemVer](https://semver.org/). Mỗi version là một tag `vX.Y.Z`, đồng thời là `version`/`appVersion` của
chart và tag image `docker.io/binhphuong/rke-nodegroup-autoscaler:X.Y.Z`.

## [0.2.6] - 2026-10-09

### Added
- `nodeCerts.hostPath` (khuyến nghị, ví dụ `/etc/kubernetes/ssl`): provider đọc cert node RKE thẳng từ node đang
  chạy nó (nên là master), không cần khai cert trong values hay tạo Secret. Init container `copy-node-certs`
  (root, không capability, root filesystem chỉ đọc) mount **từng file** trong 7 file cần thiết rồi copy vào volume
  RAM với quyền 0440; provider vẫn chạy non-root. `kube-ca-key.pem` và key của control plane không bao giờ được
  mount vào pod. Sau `rke cert rotate` chỉ cần `kubectl rollout restart` provider.
- `NOTES.txt` cảnh báo khi minor của `clusterAutoscaler.image.tag` khác minor Kubernetes của cluster (chỉ cảnh báo,
  không chặn).
- Chart kiểm tra `nodeCerts`: dùng đúng một nguồn (`hostPath`, `existingSecret` hoặc `files`), `hostPath` phải là
  đường dẫn tuyệt đối.

### Changed
- Leaf certificate (gRPC server của provider, gRPC client của cluster-autoscaler, bootstrap) được **giữ lại qua các
  lần upgrade** như CA, thay vì cấp mới mỗi lần render. Chỉ cấp lại khi Secret chưa có, CA/CN/SAN đổi (ví dụ đổi
  `bootstrap.nodeIPs`) hoặc cert còn dưới 30 ngày. `helmfile diff`/`apply` không đổi gì thì không còn báo 3 Secret
  TLS thay đổi. Secret có thêm annotation `rke-autoscaler.io/cert-id`, `cert-not-after`, `cert-expires`. Lần
  upgrade đầu lên bản này vẫn cấp lại một lần (Secret cũ chưa có annotation).
- README: mục "Công cụ triển khai được hỗ trợ" (cần `lookup`: Helm, helmfile, Flux được; Argo CD,
  `helm template | kubectl apply` thì không) và cách làm `helmfile diff` sạch.
- `revisionHistoryLimit: 3` cho Deployment của provider và cluster-autoscaler (mặc định 10 làm ReplicaSet cũ tích
  tụ qua các lần upgrade).
- Pod chỉ restart khi cert hoặc cấu hình nó đọc lúc khởi động thật sự đổi (`checksum/tls` cho provider,
  `checksum/grpc` cho cluster-autoscaler).

### Fixed
- Mỗi lần deploy provider, cluster-autoscaler mất kết nối tới provider 15–30 giây (`connection refused` /
  `i/o timeout`): readiness probe của provider giờ chạy mỗi 2 giây thay vì 10 giây.
- Đổi `clusterAutoscaler.grpcTimeout` không có tác dụng cho tới khi cluster-autoscaler tình cờ restart: giờ
  `checksum/grpc` làm nó restart.

## 0.2.5 - 2026-10-08

Chỉ phát hành image để test (`docker.io/binhphuong/rke-nodegroup-autoscaler:0.2.5`), không có git tag; mã nằm trong
commit của 0.2.6.

### Changed
- Node repair chỉ thay node khi group đang ở `minSize`. Trên `minSize`, node hỏng chỉ bị xoá (group giảm 1 node) và
  Cluster Autoscaler tự scale up nếu workload cần. Trước đó repair luôn thay node, nên group `minSize: 0` không có
  workload vẫn bị tạo một VM thừa rồi phải chờ Cluster Autoscaler thu hồi (thấy khi test stop VM trên cluster test).
- `values.yaml`: giải thích chi tiết từng option (tiếng Việt), kèm giá trị mặc định upstream của cluster-autoscaler.
- `docs/node-repair.md`: thời gian các trường hợp lấy từ lần test thật (kubelet dừng 13,5 phút, stop VM 4,5 phút,
  xoá VM 5 phút, tính tới lúc node mới Ready).

### Fixed
- Log `deleting orphan node whose VM is gone` bị ghi trùng ngay sau khi một node vừa được xoá theo cách bình thường.
- Log `node registered` chỉ có trường `replaces` khi node đó là node thay thế.

## 0.2.4 - 2026-10-08

Chỉ phát hành image để test (`docker.io/binhphuong/rke-nodegroup-autoscaler:0.2.4`), không có git tag; mã nằm trong
commit của 0.2.6.

### Added
- **Node repair:** provider tự thay node hỏng của node group mà giữ nguyên target size, nên chạy được cả khi group
  ở `minSize`/`maxSize` và không bị backoff của Cluster Autoscaler. Chi tiết: [docs/node-repair.md](docs/node-repair.md).
  - VM còn chạy, node NotReady quá `repair.notReadyAfter` (10m): tạo node mới trước, xoá node cũ khi node mới Ready.
  - VM bị xoá, stop hoặc `ERROR`, node NotReady quá `repair.vmGoneAfter` (2m): xoá và tạo song song.
  - Node cũ hồi phục trong lúc thay thì được giữ lại và node thay thế bị huỷ (target size không vượt `maxSize`);
    node thay thế hỏng thì giữ node cũ và thử lại sau `repair.retryAfter` (30m); tạo VM lỗi thì thử lại sau 5 phút.
  - Thời gian NotReady tính từ lần đầu thấy NotReady (`notReadySince` trong state), không bị reset khi Ready chuyển
    `False` → `Unknown`.
  - Cầu chì `repair.maxUnhealthyPercent` (20%) theo group và theo cả cluster (kiểm tra cluster luôn dùng giá trị
    cấp chart); mỗi group chỉ thay một node một lúc.
  - `DecreaseTargetSize` không huỷ node thay thế của repair.
  - Cấu hình `repair` ở cấp chart, ghi đè được theo từng node group.
- Phase `Replacing` trong state, báo cho Cluster Autoscaler là `instanceDeleting`; các trường `replacedBy`,
  `replaces`, `repairAfter`, `notReadySince`.
- Trạng thái VM `STOPPED`/`SHUTOFF` của VNG Cloud được nhận diện (`cloud.PhaseStopped`).
- `scripts/release-image.sh` (`make push`): build image multi-arch ở máy và push lên Docker Hub, chặn ghi đè version
  đã có, kiểm tra version của chart.
- Tài liệu [docs/vm-image.md](docs/vm-image.md): dựng image VM có sẵn Docker và image cho node group.

### Changed
- Node object bị mất trong khi VM còn (lỗi 3 của 0.2.3) giờ được thay bằng node repair thay vì chuyển `Failed`,
  nên không còn backoff 5 phút. Khi `repair.enabled: false`, hành vi cũ được giữ nguyên.
- Chart: thêm `scale-down-unready-time: 10m` vào `clusterAutoscaler.extraArgs` (mặc định upstream 20m).

## [0.2.3] - 2026-10-08

### Fixed
- `DeleteNodes` từ chối xoá instance `Failed` khi group đang ở `minSize`, dù instance đó không nằm trong target
  size. Với `enforce-node-group-min-size`, VM lỗi bị kẹt lại mãi.
- `DecreaseTargetSize` có thể huỷ instance đang tạo làm group xuống dưới `minSize`, khiến VM bị tạo rồi huỷ liên tục.
- Instance `Running` mất node object trong khi VM còn được tính vào target size mãi mãi. Giờ chuyển `Failed` sau
  5 phút để Cluster Autoscaler thay.

### Changed
- `GPULabel` trả label rỗng thay vì một label tự đặt.
- Một version duy nhất cho tag, chart và image; `make check-version` (CI chạy khi có tag) chặn khi lệch.

## [0.2.2] - 2026-10-08

### Changed
- Bỏ `helmfile.yaml` và `helm_vars/` khỏi git (chứa thông tin cluster và bí mật).
- `version` và `appVersion` của chart cùng là 0.2.2.

## [0.2.1] - 2026-10-07

### Added
- `scripts/update-worker-template.sh`: cập nhật `workerTemplate.json` từ một worker đang chạy, có chế độ `--check`.
- Chart: tuỳ chọn tạo Secret cert node RKE từ values (`nodeCerts.files`).

### Fixed
- RBAC của Cluster Autoscaler thiếu quyền list/watch `volumeattachments` (cần từ CA 1.32).
- Bật `enforce-node-group-min-size` mặc định: không có nó, CA không tự tạo đủ `minSize` node.
- `GPULabel` trả `Unimplemented` làm CA ghi log lỗi mỗi vòng lặp.

## [0.2.0] - 2026-10-07

### Added
- Cloud-init của site (`bootstrap.cloudConfig`, `bootstrap.initScripts`) chạy trước script join, gửi dưới dạng
  user_data MIME multipart.
- Chart: tuỳ chọn tạo Secret credentials VNG Cloud từ values.

### Fixed
- Tên resource sinh ra quá dài (Service > 63 ký tự) với một số tên release.

## [0.1.0] - 2026-10-07

### Added
- `nodegroup-provider`: provider externalgrpc của Cluster Autoscaler cho RKE1 trên VNG Cloud. Tạo VM với token
  bootstrap một lần, VM tự lấy script join qua HTTPS, state lưu trong ConfigMap, reconcile, dọn node mồ côi.
- Helm chart: provider, Cluster Autoscaler 1.32.7, mTLS cho gRPC, ValidatingAdmissionPolicy chỉ cho xoá node có
  label của group, NetworkPolicy, RBAC tối thiểu.
- Preflight: kiểm tra bộ cert node, CA của cluster, template worker so với cluster thật.
- CI: test, lint chart; khi có tag thì build image amd64/arm64 và đẩy chart OCI.

[0.2.6]: https://github.com/0xphuong/rke-nodegroup-autoscaler/compare/v0.2.3...v0.2.6
[0.2.3]: https://github.com/0xphuong/rke-nodegroup-autoscaler/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/0xphuong/rke-nodegroup-autoscaler/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/0xphuong/rke-nodegroup-autoscaler/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/0xphuong/rke-nodegroup-autoscaler/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/0xphuong/rke-nodegroup-autoscaler/releases/tag/v0.1.0
