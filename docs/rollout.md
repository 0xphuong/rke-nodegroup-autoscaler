# Rollout: thay dần node khi đổi cấu hình máy

Từ **v0.4.0**. Khi đổi cấu hình máy của một node group (ví dụ `flavorId` 2x8 → 2x16), provider tự thay từng node cũ
bằng node mới, không cần drain hay xoá tay.

Cloud không resize VM đang chạy. Provider tạo VM mới với cấu hình mới rồi bỏ VM cũ.

## 1. Cái gì được tính là "đổi cấu hình máy"

Mỗi instance lưu một hash cấu hình (trường `spec` trong ConfigMap `<release>-provider-state`). Hash được tính lúc
tạo, từ khối `vngcloud:` hoặc `ucloud:` của group **sau khi đã gộp giá trị chung**.

| Đổi | Thay node? |
|---|---|
| VNG Cloud: `flavorId`, `imageId`, `rootDiskTypeId`, `rootDiskSize`, `networkId`, `subnetId`, `securityGroups`, `sshKeyId`, `serverGroupId`, `zoneId` | Có |
| UCloud: `zone`, `imageId`, `machineType`, `minimalCpuPlatform`, `cpu`, `memoryMB`, `bootDiskType`, `bootDiskSize`, `vpcId`, `subnetId`, `securityGroupId`/`secGroupIds`, `keyPairId`, `isolationGroup` | Có |
| `tags`, UCloud `chargeType`, `businessGroup` | Không |
| `labels`, `taints`, `resources`, `minSize`/`maxSize` của group | Không |
| Version Kubernetes (worker template) | Không (xem `docs/plan.md`, mục 2) |

Đổi giá trị chung trong `vngcloud:`/`ucloud:` cũng tính, cho mọi group lấy giá trị đó.

## 2. Các bước thay một node

1. Provider chọn node **cũ nhất** có hash lệch. Node đó chuyển sang `Replacing`; Cluster Autoscaler thấy nó đang bị
   xoá nên không đếm và không scale down nó. Node cũ **vẫn chạy pod bình thường**.
2. Provider tạo VM mới với cấu hình mới. Target size không đổi, nên trong lúc thay group có thể có thêm 1 VM, kể cả
   khi đang ở `maxSize`.
3. Node mới Ready → provider **cordon** node cũ và **evict** pod của nó qua Eviction API, giống `kubectl drain`:
   - PodDisruptionBudget được tôn trọng: eviction bị từ chối thì thử lại ở vòng reconcile sau (30 giây).
   - Pod của DaemonSet, static pod, pod đã kết thúc: để nguyên.
   - **Pod không có controller** (tạo tay, không thuộc Deployment/StatefulSet/Job...): **không evict**, vì sẽ mất
     hẳn. Drain chờ đến khi bạn tự xoá pod đó.
   - Pod dùng `emptyDir`: bị evict, dữ liệu `emptyDir` mất (như `kubectl drain --delete-emptydir-data`).
4. Hết pod cần chuyển → xoá VM cũ, rồi xoá Node object.
5. Sang node lệch tiếp theo.

Node cũ bị NotReady hoặc mất trong lúc đó thì bị xoá luôn, không drain.

## 3. Khi nào rollout chạy, khi nào dừng

Mỗi group chỉ thay **một node một lúc**. Một bước mới chỉ bắt đầu khi mọi instance của group là node Running và
Ready. Cụ thể, rollout chờ khi:
- đang scale up (có instance `Creating`);
- đang repair, hoặc đang có một bước rollout khác;
- có instance `Failed` (chờ Cluster Autoscaler xoá);
- có node của group bị NotReady (để repair xử lý trước).

**Node mới không join được** (quá `maxProvisionTime`): node cũ được giữ lại và đưa về phục vụ bình thường. Lần thử
sau cho node đó chờ `repair.retryAfter` (mặc định 30m). Vì có instance `Failed`, rollout của cả group dừng tới khi
Cluster Autoscaler xoá nó. Nên sửa cấu hình sai (image, flavor hết quota...) thay vì để nó thử lại mãi.

**Drain bị chặn** (PDB, pod không có controller): node cũ ở trạng thái cordon và chờ mãi. Provider log cảnh báo mỗi
10 phút kèm danh sách pod bị chặn. Không có timeout ép xoá.

## 4. Cấu hình

```yaml
rollout:
  enabled: true          # mặc định bật cho mọi group

nodeGroups:
  - name: database
    rollout: {enabled: false}   # tắt riêng group này
```

Tắt rồi bật lại thì các node lệch được thay từ lúc bật.

## 5. Quy trình đổi flavor

1. Sửa cấu hình máy của group (`flavorId`, hoặc `machineType`/`cpu`/`memoryMB` với UCloud).
2. **Sửa `resources` cho khớp máy mới.** Cluster Autoscaler dùng nó khi group không có node.
3. Kiểm tra PodDisruptionBudget của workload chạy trên group. Một PDB `maxUnavailable: 0`, hoặc `minAvailable`
   bằng số replica, sẽ chặn drain mãi.
4. `helmfile diff` rồi `helmfile apply`. Provider restart vì ConfigMap đổi, rồi bắt đầu thay node.
5. Theo dõi:
   ```bash
   kubectl --context <ctx> -n <ns> logs deploy/<release>-provider -f | grep -E 'replacing instance|draining|evicted|drain blocked|drained|replacement failed'
   kubectl --context <ctx> get nodes -l rke-autoscaler.io/nodegroup=<group>
   ```

Thời gian mỗi node: khoảng thời gian join (1–2 phút với image dựng sẵn) cộng thời gian pod rời node cũ.

## 6. Nâng cấp lên 0.4.0

Instance tạo trước 0.4.0 không có hash. Lúc khởi động, provider gán cho chúng hash của cấu hình **hiện tại**
(log `instance adopted the current machine spec`), nên nâng cấp không thay node nào.

Hệ quả: nếu đổi cấu hình máy **trong cùng lần apply** lên 0.4.0, các node cũ không được thay. Hãy apply 0.4.0 trước
với cấu hình cũ, rồi mới đổi cấu hình máy.

## 7. Quyền

Provider cần thêm `patch nodes` (cordon), `list pods`, `create pods/eviction`. Provider không bao giờ xoá pod trực
tiếp; mọi pod rời node đều qua Eviction API.
