# Node group trên UCloud (UHost)

Từ **v0.3.1**, provider tạo VM trên UCloud qua [ucloud-sdk-go](https://github.com/ucloud/ucloud-sdk-go)
(`services/uhost`). Mọi phần khác (cluster-autoscaler, bootstrap, node repair, chart) dùng chung với VNG Cloud.
Tài liệu này chỉ ghi những gì riêng cho UCloud.

> Trạng thái: driver mới có unit test với API giả lập, **chưa chạy với UCloud thật**. Lần đầu nên test trên một
> cluster không phải production.

## 1. Điều kiện

- **Cluster RKE1 chạy trên UCloud**, cùng region với node group (ví dụ Singapore `sg`). VM mới phải gọi được:
  - API server `6443` của control plane;
  - NodePort bootstrap (mặc định `31443`) trên các node trong `bootstrap.nodeIPs`;
  - các port worker của RKE và CNI, giống các worker hiện có.

  Đặt node group **cùng VPC và subnet** với cluster cho đơn giản.
- **Image** của node group: Ubuntu đã cài Docker đúng version của cluster, đã pull sẵn image RKE, và **bật
  cloud-init** (UCloud chỉ nhận `UserData` khi image hỗ trợ cloud-init). Cách dựng: [vm-image.md](vm-image.md).
  Image id **khác nhau giữa các zone** (`sg-01`, `sg-02`): lấy đúng id của zone đặt trong node group.
- **API key** của một tài khoản con chỉ có quyền UHost: `CreateUHostInstance`, `DescribeUHostInstance`,
  `PoweroffUHostInstance`, `TerminateUHostInstance`. Không cần quyền nào khác.

## 2. Values

Mỗi cloud dùng **một file values độc lập**, đầy đủ mọi khối (ví dụ `values-vngcloud.yaml` và `values-ucloud.yaml`,
để ngoài git). Không gộp hai cloud vào một file: mỗi release chỉ chạy một `cloudProvider`. Phần riêng của UCloud:

Tham số `CreateUHostInstance` khai báo **một lần trong `ucloud:`** và dùng chung cho mọi node group (từ **v0.3.2**).
Khối `ucloud:` trong từng group là tuỳ chọn, chỉ để ghi đè: trường nào group không khai báo (hoặc để `""`/`0`) thì
lấy giá trị chung, `tags` được gộp (trùng key thì group thắng). Bảo mật lấy nguyên cụm: group không khai báo cả
`secGroupIds` lẫn `securityGroupId` thì lấy cả hai từ `ucloud:`; `secGroupIds: []` nghĩa là không gắn security group.

```yaml
cloudProvider: ucloud
ucloud:
  region: sg
  projectId: org-xxxxxx          # bắt buộc với tài khoản con
  # tham số UHost chung
  zone: sg-02                    # image id khác nhau giữa các zone
  imageId: uimage-xxxxxx
  machineType: "N"               # N, C, O, OM, ...; "N" phải để trong ngoặc kép (YAML đọc N trần thành false)
  cpu: 2
  memoryMB: 4096
  bootDiskType: CLOUD_SSD
  bootDiskSize: 20
  vpcId: uvnet-xxxxxx
  subnetId: subnet-xxxxxx
  secGroupIds: [secgroup-xxxxxx] # hoặc securityGroupId: firewall-xxxxxx (kiểu cũ)
  keyPairId: ""                  # tuỳ chọn
  chargeType: Dynamic            # mặc định
  credentials:
    existingSecret: ucloud-credentials

nodeGroups:
  - name: small                  # dùng toàn bộ tham số chung
    minSize: 0
    maxSize: 3
    resources: {cpu: "2", memory: 3700Mi, ephemeralStorage: 18Gi}   # allocatable thật của 1 VM
  - name: big                    # chỉ ghi đè cấu hình máy
    minSize: 0
    maxSize: 2
    resources: {cpu: "4", memory: 15Gi}
    ucloud: {machineType: O, cpu: 4, memoryMB: 16384}
```

Tạo Secret credentials (không để key trong values khi dùng thật):

```bash
kubectl --context <ctx> -n kube-system create secret generic ucloud-credentials \
  --from-literal=publicKey=... --from-literal=privateKey=...
```

Giải thích đầy đủ từng trường: comment trong `charts/rke-nodegroup-autoscaler/values.yaml`. Ví dụ dùng cho CI:
`charts/rke-nodegroup-autoscaler/ci/test-values-ucloud.yaml`.

Phần không phụ thuộc cloud làm như với VNG Cloud (README, mục Cài đặt): `nodeCerts.hostPath` + provider ghim lên
master (worker template provider tự đọc từ master đó, không cần file), `bootstrap.nodeIPs` là IP nội bộ của các
master.

## 3. Driver làm gì

| Việc | API UCloud | Ghi chú |
|---|---|---|
| Tạo VM | `CreateUHostInstance` | Tên VM = tên node. `UserData` (base64, tối đa **16 KB**) chứa script bootstrap; vượt quá thì báo lỗi rõ ràng trước khi gọi API. Label `rke-autoscaler-cluster`, `rke-autoscaler-nodegroup` và `tags` của group gắn lên UHost. Boot disk là disk duy nhất. |
| Đăng nhập | `LoginMode` | `keyPairId` có thì dùng `KeyPair`; không thì một mật khẩu ngẫu nhiên 24 ký tự mà **không ai giữ**. SSH vào VM qua key đặt trong `bootstrap.cloudConfig`. |
| Mạng | `VPCId`, `SubnetId`, `SecurityMode` | `secGroupIds` → chế độ `SecGroup` (tối đa 5, theo thứ tự ưu tiên); `securityGroupId` → chế độ `Firewall`. |
| Tính tiền | `ChargeType` | Mặc định `Dynamic` (theo giờ): node autoscale sống vài giờ, không nên trả theo tháng. |
| Xem trạng thái | `DescribeUHostInstance` | Lọc theo `UHostIds`; danh sách rỗng thì driver coi là "VM đã mất". **Cần xác nhận khi test thật:** nếu UCloud trả về mã lỗi thay vì danh sách rỗng cho id đã xoá, record sẽ kẹt ở `Deleting` (log `ucloud describe ...`). |
| Xoá VM | `PoweroffUHostInstance` → `TerminateUHostInstance` | UCloud **chỉ xoá VM đã tắt**. Lần gọi đầu tắt máy (`Poweroff`), lượt reconcile sau (≤ 30s), khi VM đã `Stopped`, thì `Terminate` kèm `ReleaseUDisk` và `ReleaseEIP`. |

Trạng thái UHost được hiểu như sau (ảnh hưởng tới node repair, xem [node-repair.md](node-repair.md)):

| State của UHost | Provider coi là | Repair |
|---|---|---|
| `Running` | chạy | ngưỡng `repair.notReadyAfter` (VM còn sống) |
| `Initializing`, `Starting`, `Rebooting`, rỗng | đang tạo / còn sống | như trên, **reboot không bị coi là VM chết** |
| `Stopping`, `Stopped` | VM đã tắt | ngưỡng `repair.vmGoneAfter` |
| `Install Fail` | lỗi | VM đang tạo thì báo lỗi cho cluster-autoscaler; VM đang chạy thì như VM đã tắt |

## 4. Test đề xuất

Theo thứ tự, trên cluster test, luôn với `--context` của cluster đó:

1. `helm upgrade` với `minSize: 1`: một UHost được tạo, log provider có `instance created`, `bootstrap served`,
   `node registered`; node Ready.
2. Đặt `minSize: 0`: sau ~10 phút cluster-autoscaler scale down; log có `deleting instance`, VM chuyển `Stopped`
   rồi biến mất khỏi console, node object bị xoá.
3. Scale up theo tải: `deploy/<cluster>/test-deployment.yaml` (chỉnh `nodeSelector` cho group mới).
4. Repair: stop UHost trên console khi group ở `minSize` → sau ~2,5 phút VM bị thay.

Lỗi thường gặp:
- VM tạo xong nhưng không có `bootstrap served`: image chưa bật cloud-init, hoặc security group/firewall chặn
  `31443` tới master. Xem `/var/log/cloud-init-output.log` và `/var/log/rke-nodegroup-bootstrap.log` trên VM.
- `ucloud create ...: RetCode ...`: sai zone/image (image id theo zone), dòng máy không có ở zone đó, hoặc API key
  thiếu quyền. Thông báo lỗi của UCloud nằm trong log provider.
