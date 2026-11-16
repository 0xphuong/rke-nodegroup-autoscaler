# Plan: việc để làm sau

Các việc đã bàn nhưng chưa làm. Thứ tự gần đúng theo mức cần thiết.

## 1. Tự phát hiện template lệch sau `rke up`

Từ 0.3.3 provider đọc worker template từ control plane **một lần lúc khởi động**. Sau `rke up` làm thay đổi cluster
(upgrade k8s, đổi `extra_args`, thêm/bớt control plane), provider giữ template cũ cho tới khi `rollout restart`.

- Định kỳ so template với cluster qua API Kubernetes (không cần Docker socket thường trực):
  `kubeletVersion` của node control plane với image kubelet của template, danh sách IP control plane với `CP_HOSTS`.
- Khi lệch, chọn một trong hai (cần quyết định):
  - **tự restart**: log lý do rồi thoát, pod restart, init container đọc lại template mới;
  - **chỉ cảnh báo**: log/Event `WorkerTemplateStale`, người vận hành tự `rollout restart`.
- Đổi `extra_args` mà không đổi version thì API không thấy. Cân nhắc lưu hash của `docker inspect` lúc khởi động
  và so lại bằng một CronJob ngắn hoặc lúc mỗi lần tạo node.

## 2. Thay dần node cũ khi worker template đổi

Đã làm ở 0.4.0 cho **cấu hình máy** (flavor, image, disk, network; `docs/rollout.md`). Còn lại:

- Đưa worker template (version kubelet, `CP_HOSTS`) vào hash: sau upgrade k8s hay đổi control plane, node đang chạy
  giữ kubelet cũ và `nginx-proxy` với `CP_HOSTS` cũ. Cần quyết định có tính labels/taints của group không.
- Timeout cho drain bị PDB chặn (hiện chờ mãi, chỉ log cảnh báo mỗi 10 phút).

## 3. Preflight cho chế độ control plane

- Kiểm tra `CP_HOSTS` (template ↔ cluster) luôn đúng khi template dựng từ control plane: giữ cho chế độ ghi đè,
  bỏ hoặc đổi thành cảnh báo node cũ có `CP_HOSTS` lệch (nối với mục 2).
- `system_images.nginx_proxy` có thể khác `rke-tools`: hiện nginx-proxy dùng image của `service-sidekick`. Cân nhắc
  cho ghi đè image qua values.

## 4. Script `update-worker-template.sh`

Chỉ còn dùng cho chế độ ghi đè. Quyết định: giữ (kèm test) hay bỏ hẳn cùng `workerTemplate.json`/`existingConfigMap`.

## 5. `rke etcd snapshot-restore`

`RemoveOldNodes` xoá mọi Node NotReady không có trong `cluster.yml`, gồm node của node group. Viết hướng dẫn (hoặc
cờ) tạm dừng repair và cluster-autoscaler trước khi restore.

## 6. UCloud

- VM đầu tiên tạo được nhưng không gọi tới bootstrap server: kiểm tra lại với image dựng sẵn mới, SSH vào VM xem
  `cloud-init status --long`, `/var/log/cloud-init-output.log`, `/var/log/rke-nodegroup-bootstrap.log`,
  `cloud-init query userdata` (UserData có bị base64 hai lần không).
- Thêm mục UCloud (custom image, copy image giữa zone) vào `docs/vm-image.md`.

## 7. Provider restart định kỳ trên non-prod

Bản 0.3.1 trên non-prod restart 38 lần trong 22 ngày, khoảng 3 giờ một lần, exit code 0 không log lỗi: kubelet
gửi SIGTERM. Staging cũng vậy: provider và cluster-autoscaler đều đúng 35 lần trong 22 ngày, cùng một node
control plane, nên nhiều khả năng nguyên nhân ở node (Docker/kubelet restart định kỳ) chứ không phải liveness probe. Xem lại sau khi lên 0.3.3; nếu còn thì đọc log
kubelet trên node chạy provider (`docker logs kubelet | grep <pod> | grep -i probe`) và log lý do thoát trong
provider.

## 8. Vận hành

- Đồng bộ chart sang repo triển khai khác dùng bản copy của chart, rồi bỏ các file worker template cũ ở đó.
- Xoay vòng các credential đã lộ trong quá trình làm (API key UCloud, mật khẩu trong values cũ).
