# Node repair: khi worker bị hỏng

Tài liệu này mô tả **mọi trường hợp worker của node group bị lỗi**: ai phát hiện, sau bao lâu, làm gì, và khi nào
có node mới. Phần "node repair" có từ **v0.2.4**.

Chỉ áp dụng cho node do provider tạo (có label `rke-autoscaler.io/nodegroup` và có record trong state). Control
plane, worker do RKE quản lý và node join bằng tay **không bao giờ** bị đụng tới.

## Mục lục

1. [Tóm tắt](#1-tóm-tắt)
2. [Các mốc thời gian dùng trong tài liệu](#2-các-mốc-thời-gian-dùng-trong-tài-liệu)
3. [Vì sao provider tự sửa thay vì để Cluster Autoscaler](#3-vì-sao-provider-tự-sửa-thay-vì-để-cluster-autoscaler)
4. [Hai cách thay: tạo trước, hoặc xoá và tạo song song](#4-hai-cách-thay-tạo-trước-hoặc-xoá-và-tạo-song-song)
5. [Các trường hợp chi tiết](#5-các-trường-hợp-chi-tiết)
6. [Cầu chì an toàn](#6-cầu-chì-an-toàn)
7. [Phối hợp với Cluster Autoscaler](#7-phối-hợp-với-cluster-autoscaler)
8. [Cấu hình](#8-cấu-hình)
9. [Theo dõi](#9-theo-dõi)
10. [Cách test từng trường hợp](#10-cách-test-từng-trường-hợp)
11. [So sánh với EKS](#11-so-sánh-với-eks)
12. [Giới hạn đã biết](#12-giới-hạn-đã-biết)

## 1. Tóm tắt

**Quy tắc chung của repair:**
- Group đang **ở `minSize`**: node hỏng được **thay** (giữ nguyên số node). Đây là trường hợp Cluster Autoscaler bị
  kẹt, xem [mục 3](#3-vì-sao-provider-tự-sửa-thay-vì-để-cluster-autoscaler).
- Group đang **trên `minSize`**: node hỏng chỉ bị **xoá**, group giảm 1 node. Nếu workload cần chỗ, pod chuyển
  Pending và Cluster Autoscaler scale up ngay (không backoff). Như vậy không tạo VM thừa khi group vốn sắp được
  scale down (ví dụ `minSize: 0` mà không có workload). Xem [mục 4.3](#43-group-đang-trên-minsize-chỉ-xoá).

Bảng dưới mô tả trường hợp group **ở `minSize`**. Thời gian "có node mới" lấy từ lần test thật trên cluster test
(image dựng sẵn, VM boot và join khoảng 3 phút).

| # | Tình huống | Ai xử lý | Bắt đầu thay sau | Cách thay | Có node mới sau (≈) |
|---|---|---|---|---|---|
| A | VM lỗi **lúc đang tạo** (`ERROR`) | Cluster Autoscaler | ≤ 30s | xoá, backoff, tạo lại | 10–11 phút |
| B | VM tạo xong nhưng **không Ready** trong `maxProvisionTime` | Cluster Autoscaler | 20 phút | xoá, backoff, tạo lại | 30 phút |
| C | VM chạy, **node NotReady** (kubelet/docker chết, mất mạng) | Provider | 10 phút NotReady | **tạo trước**, xoá sau | 13,5 phút (đo thật) |
| D | VM bị **xoá trên portal** | Provider | 2 phút NotReady | xoá + tạo song song | 5 phút (đo thật) |
| E | VM bị **stop** | Provider | 2 phút NotReady | xoá + tạo song song | 4,5 phút (đo thật) |
| F | VM chuyển **`ERROR`** khi đang chạy | Provider | 2 phút NotReady | xoá + tạo song song | ~5 phút |
| G | **Node object bị xoá** (`kubectl delete node`), VM còn | Provider | 5 phút | xoá + tạo song song | ~9 phút |
| H | **Cả VM và node object** đều mất | Provider bỏ record; CA tạo bù | ≤ 30s | tạo mới (min size / pod Pending) | 5–6 phút |
| I | Node **hồi phục** trong lúc đang thay | Provider | — | giữ node cũ, huỷ node thay thế | — |
| J | Node thay thế **cũng hỏng** | Provider + CA | — | giữ node cũ, thử lại sau 30 phút | — |
| K | **Tạo VM thay thế bị lỗi** (quota, API) | Provider | — | thử lại sau 5 phút | — |
| L | **Nhiều node cùng hỏng** | Cầu chì | — | không thay, chỉ ghi log | — |
| M | **Hai node** hỏng cùng lúc (dưới ngưỡng cầu chì) | Provider | — | thay lần lượt từng node | — |
| N | **Provider restart** giữa chừng | Provider | — | tiếp tục từ state ConfigMap | — |
| O | **Reboot** VM | Không ai | — | không thay (reboot < 10 phút) | — |

"Có node mới sau" tính từ lúc sự cố xảy ra tới lúc node thay thế `Ready` (image dựng sẵn Docker và image, xem
[vm-image.md](vm-image.md)). Với C–G khi group đang **trên** `minSize`: node hỏng bị xoá ở cùng mốc "bắt đầu thay",
không có node mới.

## 2. Các mốc thời gian dùng trong tài liệu

| Mốc | Giá trị | Ở đâu |
|---|---|---|
| Node chuyển NotReady sau khi kubelet im lặng | 40–50 giây | `node-monitor-grace-period` của kube-controller-manager (mặc định đổi theo version Kubernetes) |
| Pod bị evict khỏi node NotReady | 5 phút | toleration `not-ready`/`unreachable` mặc định 300s của pod |
| Provider reconcile | 30 giây | `provider.reconcileInterval` |
| Cluster Autoscaler quét | 10 giây | `--scan-interval` mặc định |
| VM boot và join | ~5 phút | đo trên cluster test |
| `repair.notReadyAfter` | 10 phút | VM còn chạy |
| `repair.vmGoneAfter` | 2 phút | VM đã xoá / stop / lỗi |
| `nodeLostAfter` | 5 phút | node object mất, VM còn (hằng số trong code) |
| `repair.retryAfter` | 30 phút | sau khi node thay thế hỏng |
| thử lại khi tạo VM lỗi | 5 phút | hằng số `createRetryAfter` |
| `maxProvisionTime` | 20 phút | VM không thành node Ready |
| Backoff của Cluster Autoscaler | 5 → 10 → 20 → 30 phút | `--initial-node-group-backoff-duration`, `--max-node-group-backoff-duration`; reset sau 3 giờ không lỗi |
| `scale-down-unready-time` | 10 phút | `clusterAutoscaler.extraArgs` |
| `scale-down-unneeded-time` / `scale-down-delay-after-add` | 10 phút / 10 phút | `clusterAutoscaler.extraArgs` |

## 3. Vì sao provider tự sửa thay vì để Cluster Autoscaler

Trước v0.2.4, node đang chạy mà hỏng thì chỉ trông vào Cluster Autoscaler (CA). CA có ba điểm yếu với trường hợp
này:

1. **Không xoá node xuống dưới `minSize`, kể cả node NotReady.** Group `minSize: 1` có đúng 1 node mà node đó
   hỏng thì CA không xoá nó. `enforce-node-group-min-size` cũng không tạo bù, vì node hỏng vẫn được đếm. Nếu
   `minSize = maxSize` thì group **kẹt mãi**.
2. **Chậm:** CA chỉ xoá node NotReady sau `scale-down-unready-time` (mặc định 20 phút, chart đặt 10 phút), và chỉ
   khi node đó "không cần thiết".
3. **Backoff:** cách duy nhất để provider nhờ CA thay một instance là báo instance đó lỗi. CA coi đó là scale-up
   thất bại và **chặn scale group đó 5 phút** (tăng dần tới 30 phút). Lỗi 3 của v0.2.3 (mất node object) bị đúng
   vấn đề này.

Node repair giải quyết cả ba: ở `minSize`, provider tự tạo VM thay thế **mà giữ nguyên target size**. Với CA, group
không đổi kích thước, không có scale-up thất bại, nên không có backoff, và chạy được cả khi `minSize = maxSize`.
Trên `minSize`, provider chỉ xoá node hỏng (nhanh hơn chờ `scale-down-unready-time`), phần còn lại để CA quyết định.

## 4. Cách xử lý: tạo trước, xoá và tạo song song, hoặc chỉ xoá

4.1 và 4.2 áp dụng khi group đang **ở `minSize`**; 4.3 khi group đang **trên `minSize`**.

### 4.1. Tạo trước (VM còn chạy)

Dùng khi VM vẫn `ACTIVE` nhưng node NotReady: kubelet hoặc Docker chết, hoặc VM mất mạng tới control plane.
Có thể chỉ là sự cố tạm, và node cũ có thể tự hồi phục, nên giữ nó tới khi node mới sẵn sàng.

```
node cũ NotReady ──(10 phút)──► record cũ: Running → Replacing (ReplacedBy = node mới)
                                record mới: Creating (Replaces = node cũ)
                                         │
                                         ▼ VM mới boot + join (~5 phút)
                                record mới: Running
                                         │
                                         ▼ lần reconcile kế tiếp (≤ 30s)
                                record cũ: Replacing → Deleting → xoá VM cũ → xoá node object cũ → xoá record
```

Trong lúc thay:
- Record cũ ở trạng thái `Replacing`: **không** tính vào target size, và được báo cho CA là `instanceDeleting`
  (CA không đếm và không chọn nó để scale down).
- Record mới ở `Creating`: tính vào target size. Target size **không đổi**.
- Tổng số VM thật trên cloud lúc này là **N + 1** (VM cũ vẫn còn). Nếu group đang ở `maxSize`, số VM vượt `maxSize`
  1 cái trong vài phút. Hãy chừa quota cloud cho 1 VM mỗi group.

### 4.2. Xoá và tạo song song (VM đã chết hoặc node object đã mất)

Dùng khi VM bị xoá, bị stop, ở trạng thái lỗi, hoặc node object đã bị xoá. Node cũ không còn phục vụ được gì nên
không có lý do giữ.

```
node NotReady + VM chết ──(2 phút)──► record cũ: Running → Replacing → Deleting (gọi xoá VM ngay)
                                      record mới: Creating
                                               │
                                               ▼ reconcile kế tiếp: VM cũ đã mất → xoá node object cũ
                                      pod trên node cũ bị pod GC xoá → được schedule lại ngay
                                               │
                                               ▼ VM mới boot + join (~5 phút) → Running
```

Lợi ích phụ: node object cũ bị xoá sớm (khoảng 2,5–3 phút sau sự cố), nên pod trên node đó được tạo lại ngay
thay vì chờ evict 5 phút.

### 4.3. Group đang trên minSize: chỉ xoá

Khi số node của group (target size) lớn hơn `minSize`, node hỏng (C–G, cùng các ngưỡng thời gian) chỉ bị xoá:

```
node hỏng quá ngưỡng ──► record: Running → Deleting (xoá VM) → xoá node object → xoá record
                         target size giảm 1, không tạo VM mới
```

- Nếu workload cần chỗ: pod trên node hỏng bị xoá theo node object, chuyển Pending, và CA scale up trong vòng
  ~10 giây (không backoff, vì đây là scale up bình thường).
- Nếu không cần (ví dụ `minSize: 0`, không có workload): group nhỏ lại, không có VM thừa phải chờ CA thu hồi.
- Ví dụ group 2 node, `minSize: 1`: node hỏng thứ nhất bị xoá (2 > 1); nếu node còn lại cũng hỏng, lúc đó group ở
  `minSize` nên nó được thay.
- Log: `removing broken instance without replacement (group above minSize)`.
- Vẫn áp dụng cầu chì ([mục 6](#6-cầu-chì-an-toàn)) và mỗi group một repair một lúc.

## 5. Các trường hợp chi tiết

Các mốc thời gian dưới đây là khi group **ở `minSize`**. Trên `minSize` xem [4.3](#43-group-đang-trên-minsize-chỉ-xoá).

Mỗi trường hợp ghi: dấu hiệu, luồng xử lý, mốc thời gian (T = lúc sự cố xảy ra), và những gì nhìn thấy.

### A. VM lỗi lúc đang tạo

- **Dấu hiệu:** VNG Cloud trả trạng thái chứa `ERROR` cho VM còn ở `Creating`.
- **Luồng:** provider chuyển record sang `Failed` và báo CA instance lỗi. CA gọi `DeleteNodes` và cho group vào
  backoff. Hết backoff, nếu vẫn dưới `minSize` hoặc còn pod Pending, CA scale up lại.
- **Thời gian:** T+30s `Failed` → T+40s xoá VM, backoff 5 phút → T+5m40s tạo VM mới → **≈ T+10–11 phút** Ready.
- **Lặp lại:** backoff tăng 5 → 10 → 20 → 30 phút. Image hoặc flavor hỏng không gây vòng lặp tạo VM liên tục.
- Không thuộc node repair (node chưa từng chạy), hành vi giữ nguyên từ v0.2.x.

### B. VM tạo xong nhưng không join được

- **Dấu hiệu:** sau `maxProvisionTime` (20 phút) chưa có node `Ready` mang đúng `providerID`. Nguyên nhân thường
  gặp: cloud-init lỗi, không gọi được bootstrap endpoint (security group chặn NodePort 31443), token hết hạn,
  không pull được image.
- **Luồng và thời gian:** giống A, nhưng bắt đầu ở T+20 phút → **≈ T+30 phút** Ready.
- **Gỡ lỗi:** SSH vào VM, xem `/var/log/rke-nodegroup-bootstrap.log`, `docker logs kubelet`.

### C. VM còn chạy, node NotReady

Nguyên nhân: kubelet hoặc Docker chết, hết RAM hoặc disk khiến kubelet treo, VM mất mạng tới control plane.

| Mốc | Sự kiện |
|---|---|
| T | kubelet ngừng báo trạng thái |
| T+40–50s | node `NotReady` (Ready=`Unknown`); provider ghi thời điểm này vào `notReadySince` |
| T+5m | pod trên node bị evict; nếu thiếu chỗ thì Pending, CA có thể scale up (nếu còn dưới `maxSize`) |
| T+10m50s | provider gọi API kiểm tra VM: vẫn `ACTIVE` → **tạo VM thay thế**, record cũ chuyển `Replacing` |
| T+16m | node mới `Ready` |
| T+16m30s | provider xoá VM cũ, rồi xoá node object cũ khi VM đã mất |

- Provider chỉ gọi API cloud cho node đã NotReady quá `vmGoneAfter` (2 phút), và chỉ khi không bị chặn bởi
  repair khác hoặc cầu chì. Lúc mọi thứ bình thường không tốn thêm request.
- Thời gian NotReady tính từ **lần đầu** provider thấy node NotReady (`notReadySince` trong state), không phải
  `lastTransitionTime` của node. `lastTransitionTime` đổi cả khi Ready chuyển `False` → `Unknown` (ví dụ Docker
  chết trước, kubelet treo sau), nếu dựa vào nó thì bộ đếm 10 phút bị bắt đầu lại. Node `Ready` lại thì
  `notReadySince` bị xoá.
- Nếu ở T+5m CA đã scale up vì pod Pending, thì tới T+10m repair vẫn tạo thêm 1 node. Node thừa sẽ được CA thu hồi
  sau `scale-down-delay-after-add` + `scale-down-unneeded-time`. Đây là cái giá chấp nhận được để không phụ thuộc
  vào việc CA có scale hay không.
- Nếu node cũ hồi phục trước T+16m: xem trường hợp I.

### D. VM bị xoá trên portal

| Mốc | Sự kiện |
|---|---|
| T | VM bị xoá |
| T+40–50s | node `NotReady` |
| T+2m50s–3m20s | provider: VM `NotFound` → record cũ `Deleting`, **tạo VM thay thế** |
| T+3m20s–3m50s | reconcile kế tiếp: VM cũ không còn → xoá node object cũ → pod GC xoá pod → pod được schedule lại |
| T+8–9m | node mới `Ready` |

- Record cũ và node object cũ được dọn sạch. Không cần làm gì bằng tay.
- Trước v0.2.4, node object cũ nằm lại ở NotReady cho tới khi CA xoá (`scale-down-unready-time`), và không bao giờ
  bị xoá nếu group đang ở `minSize`.

### E. VM bị stop

- Trạng thái VNG `STOPPED`/`SHUTOFF` được hiểu là VM đã chết. Luồng và thời gian giống D, chỉ khác là provider gọi
  xoá VM thật.
- ⚠️ **Đừng stop VM của node group để bảo trì.** Sau 2 phút nó bị xoá và thay. Muốn bảo trì một node: `kubectl drain`
  rồi để CA thu hồi, hoặc tắt repair cho group đó (`nodeGroups[].repair.enabled: false`) trong thời gian bảo trì.
  Reboot thì không sao, xem O.

### F. VM chuyển `ERROR` khi đang chạy

- Ví dụ: host vật lý lỗi, VNG Cloud báo VM `ERROR`. Giống E.
- Nếu VM `ERROR` nhưng kubelet vẫn báo Ready (hiếm), node vẫn `Ready` nên **không** thay: repair chỉ xét node
  NotReady.

### G. Node object bị xoá, VM vẫn còn

Ví dụ: ai đó chạy `kubectl delete node`. kubelet chỉ đăng ký node khi khởi động, nên node object không tự quay lại.

| Mốc | Sự kiện |
|---|---|
| T | node object bị xoá; pod GC xoá pod trên node → pod được schedule lại ngay |
| T+≤30s | provider ghi `nodeMissingSince` |
| T+5m30s | quá `nodeLostAfter` (5 phút): record cũ `Deleting` (xoá VM), **tạo VM thay thế** |
| T+10–11m | node mới `Ready` |

- Nếu trong 5 phút node quay lại (ví dụ ai đó restart kubelet), `nodeMissingSince` được xoá, không thay gì.
- Node bị gỡ label `rke-autoscaler.io/nodegroup` bằng tay **không** bị coi là mất: provider tìm node theo tên và
  `providerID`.
- Với repair tắt: record chuyển `Failed`, CA xoá và backoff như v0.2.3.

### H. Cả VM và node object đều mất

- Provider thấy node object không còn, gọi API thấy VM `NotFound` → bỏ record (không có gì để dọn).
- Target size giảm 1. Nếu group dưới `minSize`, CA tạo bù ngay (không backoff); nếu không, CA chỉ tạo khi có pod
  Pending. **≈ T+5–6 phút** có node mới.

### I. Node cũ hồi phục trong lúc đang thay

Chỉ xảy ra với cách "tạo trước" (C).
- Node cũ `Ready` lại trước khi node thay thế `Ready` → record cũ về `Running`, **node thay thế bị huỷ** (xoá VM).
- Lý do huỷ thay vì giữ làm node thừa: nếu giữ, cả hai đều được tính vào target size, group đang ở `maxSize` sẽ báo
  `maxSize + 1` cho CA. Huỷ thì target size không bao giờ vượt quá giá trị trước khi repair.
- Nhận ra node cũ cả khi nó đã bị gỡ label của group (tìm theo tên và `providerID`).
- Nếu node thay thế `Ready` trước, node cũ bị xoá dù vừa hồi phục.
- Node chập chờn (Ready rồi lại NotReady) bắt đầu một lượt đếm mới từ đầu.

### J. Node thay thế cũng hỏng

- Node thay thế không `Ready` trong `maxProvisionTime` (20 phút) → record mới `Failed`.
- Reconcile kế tiếp: record cũ về `Running`, ghi `repairAfter = now + retryAfter` (30 phút) và lý do vào `message`.
- CA thấy instance `Failed` → xoá VM thay thế, backoff group.
- Sau 30 phút, nếu node cũ vẫn NotReady, provider thử thay lại. Image hoặc cloud-init hỏng không gây vòng lặp tạo
  VM liên tục.

### K. Tạo VM thay thế bị lỗi

- Ví dụ: hết quota, API VNG Cloud lỗi. Lời gọi tạo VM trả lỗi ngay.
- Record cũ về `Running` với `repairAfter = now + 5 phút`, không để lại record rác. Sau 5 phút thử lại.
- Với node object bị mất (G), mốc `nodeMissingSince` được giữ nguyên, nên lần thử lại chạy ngay khi hết 5 phút,
  không phải chờ thêm `nodeLostAfter`.
- Nếu cả việc ghi lại state cũng lỗi, record cũ nằm ở `Replacing` trỏ tới record không tồn tại; xem N.

### L. Nhiều node cùng hỏng

Xem [mục 6](#6-cầu-chì-an-toàn). Provider không thay, chỉ ghi log `repair blocked by the circuit breaker`. Khi số
node hỏng giảm xuống dưới ngưỡng (ví dụ mạng có lại), repair chạy tiếp bình thường.

### M. Hai node hỏng cùng lúc, dưới ngưỡng cầu chì

Mỗi group chỉ thay **một node một lúc**. Node thứ hai đợi cho tới khi node thay thế thứ nhất `Ready` (cách "tạo
trước") hoặc lên `Running` (cách "song song"). Với group lớn, thời gian thay N node ≈ N × 5–6 phút.

### N. Provider restart giữa chừng

- Toàn bộ trạng thái (`Replacing`, `replacedBy`, `replaces`, `repairAfter`) nằm trong state ConfigMap, nên provider
  mới tiếp tục đúng chỗ.
- Nếu provider chết **giữa** lúc đánh dấu `Replacing` và lúc tạo VM thay thế, record cũ trỏ tới một record không tồn
  tại. Reconcile kế tiếp đưa nó về `Running` với `repairAfter = now + 5 phút` (để không gọi API tạo VM mỗi 30
  giây nếu lỗi lặp lại), sau đó repair chạy lại nếu node vẫn NotReady.

### O. Reboot VM

- Lúc reboot, VM không ở trạng thái `STOPPED`/`ERROR`, nên được coi là còn chạy và áp ngưỡng 10 phút. Reboot xong
  trong 10 phút thì không có gì xảy ra.

### Các trường hợp khác

- **Control plane hoặc API server sập:** provider không list được node, bỏ qua lượt reconcile đó, không làm gì.
- **Pod dùng volume RWO (PVC):** volume chỉ được gỡ khỏi node cũ khi node đó bị xoá hoặc sau timeout force-detach
  của Kubernetes (6 phút). Cách "song song" xoá node object sớm nên pod stateful lên lại nhanh hơn.
- **Node do RKE quản lý, control plane, node join tay:** không có record trong state, không bao giờ bị repair.

## 6. Cầu chì an toàn

Nhiều node cùng hỏng một lúc thường là do **mạng hoặc control plane**, không phải do VM. Tạo VM mới không giải
quyết được, và VM mới cũng sẽ không join được. Tệ hơn, thay hàng loạt sẽ xoá các node có thể tự hồi phục khi mạng
có lại.

Provider **không thay** khi một trong hai điều kiện sau đúng:

1. Số node hỏng của group > `max(1, tổng × maxUnhealthyPercent / 100)`, với `maxUnhealthyPercent` của group (có
   thể ghi đè). "Hỏng" là node NotReady, node object mất,
   hoặc đang `Replacing`. "Tổng" là các node `Running` + `Replacing` của group.
2. Số node NotReady của **cả cluster** (gồm control plane và worker RKE) > `max(1, tổng node × maxUnhealthyPercent / 100)`,
   luôn dùng `repair.maxUnhealthyPercent` **cấp chart**. Group ghi đè `maxUnhealthyPercent: 100` cũng không tắt
   được kiểm tra này.

Danh sách toàn bộ node của cluster chỉ được lấy khi một repair sắp bắt đầu, không lấy ở mỗi lượt reconcile.

Luôn cho phép 1 node hỏng, để group nhỏ (1–4 node) vẫn được sửa. Ví dụ với `maxUnhealthyPercent: 20`:

| Số node của group | Số node hỏng tối đa vẫn được sửa |
|---|---|
| 1–9 | 1 |
| 10–14 | 2 |
| 15–19 | 3 |
| 20 | 4 |

Ví dụ group 5 node có 2 node cùng NotReady: 2 > 1 → **không sửa**. Nếu đó thật sự là 2 VM bị xoá, hãy xử lý bằng tay
hoặc tăng `maxUnhealthyPercent` cho group đó.

## 7. Phối hợp với Cluster Autoscaler

- **`scale-down-unready-time: 10m`** (chart đặt sẵn) trùng với `notReadyAfter`. Nếu CA xoá node cũ trước thì
  `DeleteNodes` được chấp nhận (record `Replacing` nằm ngoài target size), node thay thế vẫn tiếp tục. Không có node
  thứ hai bị tạo ra. Có test cho trường hợp này.
- **Pod Pending:** CA vẫn scale up như bình thường. Repair không chặn và không thay thế việc đó.
- **CA huỷ scale up (`DecreaseTargetSize`):** CA chỉ huỷ được instance đang tạo do scale up, **không** huỷ được node
  thay thế của repair. Nếu không, CA có thể huỷ đúng node thay thế (nó thường là instance mới nhất), làm node hỏng
  bị giữ lại và repair bị chặn 30 phút.
- **Backoff:** repair không tạo backoff. Chỉ khi node thay thế bị `Failed` (J) thì CA mới backoff group.
- **`minSize`/`maxSize`:** ở `minSize` repair giữ nguyên target size; trên `minSize` nó chỉ giảm target size 1 đơn vị
  và không bao giờ xuống dưới `minSize`. Group không bao giờ vượt khỏi `[minSize, maxSize]` trong mắt CA. Riêng số
  VM thật có thể là N + 1 trong lúc "tạo trước".
- **`scale-down-unready-time`** chỉ còn tác dụng khi repair tắt: trên `minSize` repair xoá node hỏng trước (2 hoặc
  10 phút), ở `minSize` CA không xoá node.

## 8. Cấu hình

Trong `values.yaml` của chart:

```yaml
repair:
  enabled: true
  notReadyAfter: 10m        # VM còn chạy: NotReady bao lâu thì thay (tạo trước)
  vmGoneAfter: 2m           # VM xoá/stop/lỗi: NotReady bao lâu thì thay (xoá + tạo song song)
  maxUnhealthyPercent: 20   # cầu chì
  retryAfter: 30m           # chờ bao lâu để thử lại sau khi node thay thế hỏng

nodeGroups:
  - name: app
    # ...
    repair:                 # tuỳ chọn: ghi đè từng trường cho riêng group này
      enabled: false
```

Ràng buộc kiểm tra khi provider khởi động:
- `notReadyAfter` và `vmGoneAfter` ≥ 1 phút (node controller cần 40–50 giây mới đánh dấu NotReady).
- `maxUnhealthyPercent` trong khoảng 1–100.

Gợi ý chỉnh:
- Workload nhạy cảm với downtime: giảm `notReadyAfter` xuống 5m. Không nên thấp hơn, vì Docker restart hay mạng
  chập chờn sẽ gây thay VM oan.
- Tắt repair (`enabled: false`): quay lại hành vi v0.2.3. Node NotReady chỉ được CA xử lý; node object mất thì
  instance chuyển `Failed`.

## 9. Theo dõi

**Log provider** (`kubectl --context <ctx> -n <ns> logs deploy/<release>-provider`):

| Log | Ý nghĩa |
|---|---|
| `replacing instance ... deleteFirst=false` | bắt đầu thay kiểu "tạo trước" |
| `replacing instance ... deleteFirst=true` | bắt đầu thay kiểu "song song" |
| `removing broken instance without replacement (group above minSize)` | trên `minSize`: chỉ xoá node hỏng (4.3) |
| `node registered ... replaces=<cũ>` | node thay thế đã Ready (`replaces` chỉ có khi là node thay thế) |
| `replacement is Ready, deleting the old instance` | đang xoá node cũ |
| `node recovered during its repair` | trường hợp I |
| `replacement failed, keeping the instance` | trường hợp J |
| `repair waits: another repair of the group is in progress` | trường hợp M |
| `repair blocked by the circuit breaker` | trường hợp L |
| `node object is gone while its VM exists` | bắt đầu trường hợp G |

**State:**

```bash
kubectl --context <ctx> -n <ns> get cm <release>-provider-state -o jsonpath='{.data.instances\.json}' | jq .
```

Các trường liên quan: `phase` (`Replacing`), `replacedBy`, `replaces`, `repairAfter`, `notReadySince`, `nodeMissingSince`,
`message`.

## 10. Cách test từng trường hợp

Luôn dùng `--context` của cluster test. Chỉ làm trên node **do autoscaler tạo** (`-l rke-autoscaler.io/nodegroup`).

```bash
CTX=<ctx>; NS=<ns>; REL=<release>
kubectl --context $CTX get nodes -l rke-autoscaler.io/nodegroup -o wide
kubectl --context $CTX -n $NS logs deploy/$REL-provider -f | grep -E 'repair|replac|registered'
```

Các kết quả mong đợi dưới đây khi group **ở `minSize`** (ví dụ `minSize: 1` và 1 node). Muốn test 4.3 thì đặt
`minSize: 0`: node hỏng chỉ bị xoá, không có node mới.

| Trường hợp | Cách gây lỗi | Kết quả mong đợi |
|---|---|---|
| C | SSH vào node: `sudo docker stop kubelet` | sau ~10,5 phút có log `deleteFirst=false`, node mới Ready ~3 phút sau; node cũ bị xoá khi node mới Ready |
| I | như C, rồi `sudo docker start kubelet` sau khi thấy log `replacing instance` | node cũ được giữ, VM thay thế bị xoá (log `cancelling the replacement`) |
| D | xoá VM trên portal VNG Cloud | sau ~3 phút có log `deleteFirst=true`, node object cũ biến mất; node mới sau ~5 phút |
| E | stop VM trên portal | như D, VM cũ bị xoá |
| G | `kubectl --context $CTX delete node <node>` | sau ~5,5 phút VM cũ bị xoá, node mới sau ~9 phút |
| 4.3 | `minSize: 0`, stop VM | sau ~3 phút log `removing broken instance without replacement`, VM và node bị xoá, không tạo node mới |
| L | group ≥ 2 node, `docker stop kubelet` trên 2 node | log `circuit breaker`, không VM nào được tạo |
| O | `sudo reboot` trên node | node NotReady 1–2 phút rồi Ready lại, không có thay thế |

Tất cả các trường hợp trên cũng có unit test trong `internal/provider/repair_test.go` và
`internal/provider/minsize_test.go` (`make test`).

## 11. So sánh với EKS

Phần này dựa trên hiểu biết chung về AWS, chưa đối chiếu lại tài liệu mới nhất.

| | EKS | rke-nodegroup-autoscaler |
|---|---|---|
| VM chết (EC2 stop/terminate/status check fail) | ASG health check: terminate rồi launch (launch trước nếu bật instance maintenance policy) | xoá + tạo song song sau 2 phút |
| kubelet NotReady, VM còn chạy | ASG không biết. Node auto repair (managed node group / Auto Mode / Karpenter) thay sau một ngưỡng thời gian (Karpenter khoảng 30 phút), xoá rồi thay | tạo trước, xoá sau, sau 10 phút |
| Cầu chì | có (khoảng 20% node lỗi thì dừng repair) | có, 20% mặc định, theo group và theo cluster |
| Tạo trước rồi xoá | chỉ khi rolling update (surge) | cả khi repair node còn VM chạy |
| CA với node NotReady ở `minSize` | kẹt (cùng phần mềm CA) | repair xử lý, không phụ thuộc CA |

## 12. Giới hạn đã biết

- **Số VM thật có thể vượt `maxSize` 1 cái** trong lúc "tạo trước" (VM cũ chưa xoá). Target size thì không bao
  giờ vượt. Cần chừa quota cloud cho 1 VM mỗi group.
- **Node object mất mà VM bị stop/lỗi** vẫn chờ `nodeLostAfter` (5 phút), không dùng `vmGoneAfter` (2 phút).
- **Không drain node cũ:** node cũ đang NotReady nên pod trên đó đã không chạy được (hoặc đã bị evict). Trường hợp
  node cũ vừa hồi phục đúng lúc node mới Ready thì pod trên node cũ bị dừng đột ngột khi VM bị xoá.
- **Phát hiện dựa trên Ready condition của node.** Lỗi ở tầng ứng dụng (DNS trong node hỏng, CNI hỏng nhưng kubelet
  vẫn Ready) không được phát hiện.
- **Cầu chì đếm cả node ngoài group:** nếu worker RKE hoặc control plane đang NotReady nhiều, repair của mọi group bị
  chặn.
- **Trạng thái VM lúc reboot:** chưa kiểm chứng VNG Cloud trả về trạng thái gì trong lúc reboot; nếu là `STOPPED`
  quá 2 phút thì VM sẽ bị thay.
