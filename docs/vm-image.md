# Tạo image VM cho node group

Image VM có sẵn Docker và các image Kubernetes giúp node mới join nhanh: khoảng 1–2 phút, thay vì khoảng
5 phút khi phải cài Docker và pull `hyperkube` (khoảng 1.6 GB) lúc boot.

Quy trình: dựng một VM "builder" từ image Ubuntu gốc → cài và cấu hình → kiểm tra → dọn → tắt máy → snapshot
thành image → đặt `imageId` của node group trỏ vào image đó.

> Thay các biến trong tài liệu: `<builder-ip>` (IP của VM builder), `<ssh-port>`, `<user>`, `<worker>` (một
> worker RKE đang chạy tốt, dùng để đối chiếu), `<ctx>` (kube context của cluster).

## 0. Khi nào phải làm lại image

| Thay đổi trong cluster | Phải làm |
|---|---|
| Upgrade Kubernetes hoặc RKE (`hyperkube`, `rke-tools` đổi version) | Làm lại image, pull image mới |
| Đổi version CNI (canal: calico, flannel) | Làm lại image |
| Đổi version Docker trên các node | Làm lại image cho khớp |
| Thêm hoặc đổi DaemonSet chạy trên mọi node | Nên làm lại image (không bắt buộc, kubelet vẫn tự pull) |
| Chỉ đổi ứng dụng, Deployment | Không cần |

Sau khi upgrade cluster, nhớ cập nhật cả `workerTemplate` (`scripts/update-worker-template.sh`).

## 1. Dựng VM builder

- Dùng **cùng image gốc** với các worker hiện có (ví dụ Ubuntu 20.04 x64) và cùng kiến trúc (x86_64).
- Flavor nhỏ là đủ (2 vCPU / 2 GB). Root disk chỉ cần chứa các image (khoảng 20 GB). Node group có thể dùng
  disk lớn hơn, cloud-init tự mở rộng phân vùng.
- Có thể dùng cloud-config sẵn có (user, SSH key, port sshd) để vào máy.

```bash
ssh -p <ssh-port> <user>@<builder-ip>
```

## 2. Cài Docker đúng version của cluster

Lấy version Docker và cấu hình từ một worker đang chạy:
```bash
ssh <worker> 'docker version --format "{{.Server.Version}}"; sudo cat /etc/docker/daemon.json'
```

Cài `docker-ce` từ repo của Docker và **ghim đúng version** (ví dụ 28.1.1 trên Ubuntu 20.04 focal):
```bash
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
V=5:28.1.1-1~ubuntu.20.04~focal
sudo apt-get install -y docker-ce=$V docker-ce-cli=$V containerd.io
```

`daemon.json` phải giống các worker hiện có:
```bash
cat <<'EOF' | sudo tee /etc/docker/daemon.json
{
  "log-driver": "json-file",
  "log-opts": { "max-size": "512m", "max-file": "3" }
}
EOF
sudo systemctl enable docker && sudo systemctl restart docker
sudo docker info --format 'server={{.ServerVersion}} storage={{.Driver}} cgroupDriver={{.CgroupDriver}}'
```
Kết quả phải trùng với worker: `overlay2`, `cgroupfs`.

## 3. Pull sẵn image

### 3.1 Worker plane và CNI (bắt buộc)
Lấy đúng tag image đang chạy trên worker:
```bash
ssh <worker> 'sudo docker images --format "{{.Repository}}:{{.Tag}}"' | grep -E 'rancher/(hyperkube|rke-tools|mirrored-pause|mirrored-calico|calico-cni|mirrored-flannel)'
```

Ví dụ với cluster RKE v1.32.6:
```bash
for img in \
  rancher/hyperkube:v1.32.6-rancher1 \
  rancher/rke-tools:v0.1.114 \
  rancher/mirrored-pause:3.7 \
  rancher/mirrored-calico-node:v3.30.2 \
  rancher/calico-cni:v3.30.2-rancher1 \
  rancher/mirrored-flannel-flannel:v0.26.4; do
  sudo docker pull "$img"
done
```
Ba image đầu phải trùng với `workerTemplate` của chart: kubelet và kube-proxy dùng `hyperkube`, `nginx-proxy` và `service-sidekick` dùng `rke-tools`.

### 3.2 Image của các DaemonSet (nên có)
Liệt kê image của mọi DaemonSet trong cluster:
```bash
kubectl --context <ctx> get ds -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{range .spec.template.spec.containers[*]}{.image}{" "}{end}{range .spec.template.spec.initContainers[*]}{.image}{" "}{end}{"\n"}{end}'
```
Pull các image của DaemonSet **sẽ chạy trên node của group**. Một DaemonSet chỉ lên node có taint nếu nó có toleration tương ứng. Ví dụ:
```bash
sudo docker pull quay.io/prometheus/node-exporter:v1.8.1                       # mọi node
sudo docker pull registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.18.0 # CSI node driver
sudo docker pull <csi-driver-image>                                            # CSI node driver
sudo docker pull busybox:latest
sudo docker pull grafana/promtail:2.8.2
```

**Image ở registry private** (ví dụ agent bảo mật): chỉ pull nếu có credential, và **phải `docker logout` trước
khi snapshot**. Nếu không, credential sẽ nằm trong image:
```bash
cat key.json | sudo docker login -u _json_key --password-stdin https://<private-registry>
sudo docker pull <private-registry>/<image>:<tag>
sudo docker logout https://<private-registry> && rm -f key.json
```
Không pull sẵn cũng được, kubelet sẽ tự pull qua `imagePullSecrets` khi node join.

## 4. Kernel module và sysctl cho Kubernetes

```bash
cat <<'EOF' | sudo tee /etc/modules-load.d/k8s.conf
br_netfilter
overlay
EOF
sudo modprobe br_netfilter && sudo modprobe overlay

cat <<'EOF' | sudo tee /etc/sysctl.d/90-k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sudo sysctl --system >/dev/null
```
Trên worker RKE, `br_netfilter` vẫn được kube-proxy tự nạp khi chạy (`RKE_KUBE_PROXY_BR_NETFILTER=true`).
Cấu hình sẵn trong image giúp node đúng ngay từ lần boot đầu.

Swap phải tắt: lệnh `swapon --show` không được in ra gì.

## 5. Gói và dịch vụ thừa (xem lại trước khi gỡ)

| Nhóm | Gói, dịch vụ | Gợi ý |
|---|---|---|
| Công cụ build | `build-essential gcc g++ make libssl-dev zlib1g-dev libpam0g-dev python3-pip` | Gỡ: node không cần, tăng bề mặt tấn công |
| Plugin Docker | `docker-buildx-plugin docker-compose-plugin` | Gỡ: k8s không dùng |
| Debug | `tcpdump iptraf telnet iotop dnsutils net-tools aptitude rsync` | Tuỳ chính sách: tiện khi xử lý sự cố |
| Dịch vụ | `rsync.service` | Disable |
| Dịch vụ | `fail2ban.service` | Tuỳ: chỉ bảo vệ sshd, không ảnh hưởng k8s |
| Dịch vụ | `iscsi.service`, `open-iscsi.service` | **Giữ** nếu CSI block storage dùng iSCSI |
| Nên giữ | `qemu-guest-agent cloud-init openssh-server curl ca-certificates` | |

Ví dụ gỡ (chỉ chạy sau khi đã xem lại):
```bash
sudo apt-get purge -y build-essential gcc g++ make libssl-dev zlib1g-dev libpam0g-dev python3-pip \
  docker-buildx-plugin docker-compose-plugin
sudo apt-get autoremove -y
sudo systemctl disable --now rsync
```

## 6. Kiểm tra trước khi dọn

```bash
docker --version                                   # đúng version của cluster
sudo docker info --format '{{.Driver}} {{.CgroupDriver}}'   # overlay2 cgroupfs
sudo docker images --format '{{.Repository}}:{{.Tag}}' | sort
sudo docker ps -a                                  # không có container nào
lsmod | grep -E '^(br_netfilter|overlay) '
sysctl net.bridge.bridge-nf-call-iptables net.ipv4.ip_forward    # đều = 1
swapon --show                                      # rỗng
ls -d /etc/kubernetes /var/lib/kubelet /etc/cni /opt/cni /var/lib/cni 2>/dev/null   # không có gì
```

## 7. Dọn trước khi snapshot (chạy cuối cùng, liền một mạch)

Mục đích: mỗi VM tạo từ image là **một máy mới hoàn toàn**. Không được dùng chung machine-id, vì DHCP client ID
sinh từ machine-id, nên trùng machine-id có thể khiến các VM trùng IP. Cũng không được dùng chung SSH host key,
không mang theo trạng thái cloud-init, lịch sử lệnh hay mật khẩu.

```bash
# ghim Docker: không để gói nào tự nâng lên version khác với cluster
sudo apt-mark hold docker-ce docker-ce-cli containerd.io

# tắt tự động cập nhật (kể cả các timer apt chạy lúc boot làm chậm cloud-init)
sudo systemctl disable --now unattended-upgrades apt-daily.timer apt-daily-upgrade.timer
cat <<'EOF' | sudo tee /etc/apt/apt.conf.d/20auto-upgrades
APT::Periodic::Update-Package-Lists "0";
APT::Periodic::Unattended-Upgrade "0";
EOF

# khoá mật khẩu user, chỉ đăng nhập bằng SSH key
sudo passwd -l <user>

# dọn cache, lịch sử lệnh, log
sudo apt-get clean && sudo rm -rf /var/lib/apt/lists/*
sudo rm -f /home/*/.bash_history /root/.bash_history
sudo journalctl --rotate && sudo journalctl --vacuum-time=1s

# xoá SSH host key: mỗi VM tự sinh key mới khi boot
sudo rm -f /etc/ssh/ssh_host_*

# đặt lại cloud-init và machine-id
sudo cloud-init clean --logs --machine-id
# nếu cloud-init không nhận --machine-id:
#   sudo truncate -s 0 /etc/machine-id && sudo rm -f /var/lib/dbus/machine-id

history -c
sudo poweroff
```

> ⚠️ Sau khi xoá host key, **không thoát SSH rồi đăng nhập lại**, vì sshd sẽ không nhận kết nối mới.
> ⚠️ **Không bật lại VM trước khi snapshot.** Nếu lỡ bật, cloud-init sẽ sinh lại machine-id và host key. Khi đó
> phải chạy lại mục 7.

## 8. Snapshot và dùng image

1. Trên console VNG Cloud: tạo image từ VM builder đã tắt, ghi lại ID image (`img-…`).
2. Thay `nodeGroups[].vngcloud.imageId` trong values.
3. Image đã có Docker, nên **bỏ phần cài Docker trong script cloud-init** (`bootstrap.initScripts`).
   - Ví dụ, script cài `docker.io` của Ubuntu sẽ gỡ `containerd`/`runc` rồi cài Docker khác,
     **đè lên `docker-ce` có sẵn trong image**.
   - `apt-mark hold` không chặn được việc gỡ gói này.
4. `helmfile apply` (hoặc `helm upgrade`).

## 9. Kiểm tra node tạo từ image mới

Sau khi Cluster Autoscaler tạo node mới (scale up, hoặc theo `minSize`):
```bash
# thời gian join: so mốc "instance created" với "node registered"
kubectl --context <ctx> -n <ns> logs deploy/<release>-provider | grep -E 'instance created|bootstrap served|node registered'

# trên VM mới
ssh -p <ssh-port> <user>@<new-node-ip> '
  cat /etc/machine-id                    # khác image builder và khác các VM khác
  sudo ls -l /etc/ssh/ssh_host_*_key     # vừa được sinh lúc boot
  docker --version                       # đúng version của cluster
  sudo docker images | grep hyperkube    # có sẵn, không phải pull
  sudo tail -5 /var/log/rke-nodegroup-bootstrap.log'
```
Mong đợi: node Ready khoảng 1–2 phút sau `bootstrap served`, tức là không còn bước pull `hyperkube`.
