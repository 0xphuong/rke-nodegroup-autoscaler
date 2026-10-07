# rke-nodegroup-autoscaler

Managed Node Group + [Cluster Autoscaler](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler)
cho cluster **RKE1** chạy trên **VNG Cloud**, không cần `rke up` khi scale.

```
Tải tăng → HPA thêm Pod → Pod Pending
        → cluster-autoscaler (cloudProvider=externalgrpc) ──gRPC/mTLS──> nodegroup-provider
        → VNG Cloud API tạo VM (user_data chỉ chứa token dùng một lần)
        → VM lấy join script qua HTTPS (NodePort trên node có sẵn) → chạy worker plane của RKE
        → kubelet đăng ký node (label + taint + providerID) → Pod được schedule

Tải giảm → cluster-autoscaler taint + drain node → DeleteNodes
        → xoá VM → VM mất hẳn → xoá Node object
```

## Thành phần

| Thành phần | Vai trò |
|---|---|
| `cluster-autoscaler` (image upstream, không sửa) | Quyết định khi nào scale, node nào bỏ; taint và drain |
| `nodegroup-provider` (repo này) | gRPC `CloudProvider` của provider `externalgrpc`: tạo/xoá VM, cấp join script, xoá Node object, reconcile |
| Helm chart `charts/rke-nodegroup-autoscaler` | Cài cả hai, RBAC tối thiểu, mTLS, ValidatingAdmissionPolicy, NetworkPolicy |

Node mới được dựng **y hệt một worker RKE có sẵn**: provider lấy `docker inspect` của `service-sidekick`,
`nginx-proxy`, `kubelet`, `kube-proxy` trên một worker mẫu, chỉ đổi `--hostname-override`, thêm
`--node-labels`, `--register-with-taints` và `--provider-id`. Đây đúng là việc RKE làm cho một worker
(`services/workerplane.go`), chỉ là không qua `rke up`.

## Mô hình bảo mật

- **Không ai có quyền cluster-admin.**
  - Provider: `nodes` get/list/watch/delete, cùng một ConfigMap state trong namespace của nó. Không đọc được Secret.
  - Cluster Autoscaler: ClusterRole y như chart upstream.
- **Quyền xoá node bị chặn thêm** bằng ValidatingAdmissionPolicy: hai ServiceAccount này chỉ xoá được Node có
  label `rke-autoscaler.io/nodegroup`, nên không xoá được control plane hay worker do RKE quản lý.
- **Chỉ Cluster Autoscaler gọi được provider.**
  - gRPC bắt buộc mTLS: CA riêng của release, chứng chỉ do chart sinh.
  - NetworkPolicy chỉ cho pod Cluster Autoscaler vào cổng gRPC.
- **`user_data` không chứa cert hay key.** Nó chỉ chứa một token ngẫu nhiên:
  - mỗi token chỉ dùng cho một VM;
  - hết hạn sau `bootstrap.tokenTTL`;
  - bị huỷ ngay khi node đăng ký xong.

  VM dùng token để lấy join script qua TLS và kiểm tra server bằng CA nhúng trong `user_data`. Mọi lần bị từ
  chối đều trả cùng một mã 403.
- **Giới hạn còn lại:** cert `kube-node` và `kube-proxy` của RKE1 **dùng chung cho mọi node**. Ai lấy được cert từ
  một node là có quyền của một node, gồm sửa label và taint của mọi node, vì RKE1 không bật NodeRestriction
  theo từng node. Giải pháp này **cố ý không sửa RKE hay cluster hiện tại** (không đổi `cluster.yml`, không
  `rke up`), nên chấp nhận giới hạn này. Muốn bỏ nó thì phải bật TLS bootstrapping qua `extra_args` và chạy
  `rke up`, tức là thay đổi cluster, nằm ngoài phạm vi repo này.

## Cài đặt

Yêu cầu:
- Kubernetes 1.30 trở lên, để có ValidatingAdmissionPolicy GA.
- Một **image VM** có Docker cùng phiên bản với cluster. Nên pull sẵn image `rancher/hyperkube` và
  `rancher/rke-tools` để join nhanh.
- **Security group** của VM mới phải cho phép:
  - đi ra port 6443 của control plane;
  - đi ra NodePort bootstrap (mặc định 31443) trên các node trong `bootstrap.nodeIPs`;
  - các port worker của RKE và CNI, giống các worker hiện có.
- Một **service account VNG Cloud** chỉ có quyền tạo và xoá server trong project.

```bash
NS=kube-system

# 1. credentials VNG Cloud
kubectl -n $NS create secret generic vngcloud-credentials \
  --from-literal=clientId=... --from-literal=clientSecret=...

# 2. cert node RKE, lấy từ một worker có sẵn
mkdir node-certs
ssh <worker> 'sudo tar czf - -C /etc/kubernetes/ssl kube-ca.pem kube-node.pem kube-node-key.pem \
  kube-proxy.pem kube-proxy-key.pem kubecfg-kube-node.yaml kubecfg-kube-proxy.yaml' | tar xzf - -C node-certs/
kubectl -n $NS create secret generic rke-node-certs --from-file=node-certs/
rm -rf node-certs

# 3. template worker (không chứa bí mật)
ssh <worker> 'sudo docker inspect service-sidekick nginx-proxy kubelet kube-proxy' > worker-template.local.json

# 4. values: xem charts/rke-nodegroup-autoscaler/values.yaml, ví dụ đầy đủ ở ci/test-values.yaml
helm upgrade --install ngas charts/rke-nodegroup-autoscaler -n $NS \
  -f values-dev.local.yaml --set-file workerTemplate.json=worker-template.local.json
```

**Sau mỗi lần `rke up` làm thay đổi cluster** (upgrade k8s, đổi tham số kubelet, đổi control plane): lấy lại
template worker và chạy `helm upgrade`. RKE không biết các node của node group nên sẽ không upgrade chúng. Provider
cũng từ chối template có `generate_serving_certificate`, vì khi đó mỗi node cần cert riêng.

## Kiểm tra luồng autoscaling

```bash
kubectl create deployment load --image=registry.k8s.io/hpa-example --replicas=1
kubectl set resources deploy/load --requests=cpu=500m
kubectl autoscale deploy/load --cpu-percent=50 --min=1 --max=30
# tạo tải, rồi theo dõi:
kubectl get hpa,pods -w
kubectl -n kube-system get configmap cluster-autoscaler-status -o yaml
kubectl -n kube-system logs deploy/ngas-rke-nodegroup-autoscaler-provider -f
kubectl get nodes -l rke-autoscaler.io/nodegroup -w
```

Nếu pod có toleration hoặc nodeSelector, chúng phải khớp `labels` và `taints` của node group thì Cluster
Autoscaler mới chọn group đó.

Gỡ lỗi trên VM: `/var/log/rke-nodegroup-bootstrap.log`, `docker logs kubelet`.

## Hành vi cần biết

- **SDK VNG Cloud không có API liệt kê server.** Provider tự ghi lại mọi VM nó tạo vào ConfigMap
  `<release>-provider-state`, và chỉ đụng tới VM có trong đó. Vì có một writer duy nhất nên Deployment chạy
  1 replica với strategy `Recreate`.
- **VM không thành node `Ready` sau `maxProvisionTime`** thì bị báo là lỗi. Cluster Autoscaler sẽ xoá VM đó và
  tạm ngưng scale group đó một thời gian (backoff).
- **Node có label của group nhưng VM đã mất** và không có trong state thì bị xoá. Nếu VM vẫn còn, provider chỉ
  ghi log cảnh báo, không tự nhận VM đó về quản lý.
- **Scale từ 0 node:** khai báo đúng `resources` của flavor, vì Cluster Autoscaler dùng nó để mô phỏng node mới.

## Phát triển

```bash
make test           # go test -race
make chart-lint
make chart-template
make image          # cần docker daemon
```

`internal/protos` là mã sinh từ `externalgrpc.proto` của Cluster Autoscaler, lấy từ tag `cluster-autoscaler-1.32.7`
(Apache-2.0, giữ nguyên header).

## Trạng thái

PoC. Phần đã kiểm chứng:
- unit test của provider: scale up, scale down, failed, timeout, orphan, min/max;
- server bootstrap và token;
- render worker plane, kèm kiểm tra cú pháp bash cho các script sinh ra;
- mTLS gRPC chạy thật;
- `helm lint` và `helm template`;
- config do chart sinh được đọc lại bằng chính code Go;
- SAN và chain của các chứng chỉ chart sinh.

**Chưa chạy với VNG Cloud thật hay trên cluster thật.** Tên trạng thái server của vServer (`ACTIVE`, `ERROR`...),
giới hạn tên và tag VM là suy ra từ SDK. Cần một vòng test trên môi trường dev trước khi dùng thật.
