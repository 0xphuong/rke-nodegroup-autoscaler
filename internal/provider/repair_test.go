package provider

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

// atMinSize sets the group's minSize (and maxSize if needed) so that n nodes sit exactly at it: there a broken
// node is replaced, above it it is only removed.
func (e *env) atMinSize(n int) {
	e.p.Config.NodeGroups[0].MinSize = n
	e.p.Config.NodeGroups[0].MaxSize = max(e.p.Config.NodeGroups[0].MaxSize, n)
}

// runningInstance creates one instance and registers it as a Ready node.
func (e *env) runningInstance(t *testing.T) (state.Record, *corev1.Node) {
	t.Helper()
	before := map[string]bool{}
	for _, r := range e.p.Store.List("app") {
		before[r.Name] = true
	}
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.p.Store.List("app") {
		if !before[r.Name] {
			n := e.register(t, r.Name, true)
			e.p.ReconcileOnce(e.ctx)
			got, _ := e.p.Store.Get(r.Name)
			if got.Phase != state.Running {
				t.Fatalf("setup: %s is %s", r.Name, got.Phase)
			}
			return got, n
		}
	}
	t.Fatal("setup: no new instance")
	return state.Record{}, nil
}

func (e *env) setReadyStatus(t *testing.T, name string, st corev1.ConditionStatus) {
	t.Helper()
	n, err := e.kube.CoreV1().Nodes().Get(e.ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st, LastTransitionTime: metav1.NewTime(e.now)}}
	if _, err := e.kube.CoreV1().Nodes().Update(e.ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// setReady flips a node's Ready condition, as the node controller does, at the current fake time.
func (e *env) setReady(t *testing.T, name string, ready bool) {
	t.Helper()
	n, err := e.kube.CoreV1().Nodes().Get(e.ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := corev1.ConditionUnknown
	if ready {
		st = corev1.ConditionTrue
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st, LastTransitionTime: metav1.NewTime(e.now)}}
	if _, err := e.kube.CoreV1().Nodes().Update(e.ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) setVM(id string, phase cloud.Phase) {
	e.cloud.mu.Lock()
	defer e.cloud.mu.Unlock()
	e.cloud.vms[id].Phase, e.cloud.vms[id].RawStatus = phase, string(phase)
}

func (e *env) replacementOf(t *testing.T, old string) state.Record {
	t.Helper()
	for _, r := range e.p.Store.List("app") {
		if r.Replaces == old {
			return r
		}
	}
	t.Fatalf("no replacement of %s in %+v", old, e.p.Store.List("app"))
	return state.Record{}
}

func (e *env) phase(name string) state.Phase {
	r, ok := e.p.Store.Get(name)
	if !ok {
		return "gone"
	}
	return r.Phase
}

func (e *env) reportedState(t *testing.T, name string) protos.InstanceStatus_InstanceState {
	t.Helper()
	r, _ := e.p.Store.Get(name)
	nodes, err := e.p.NodeGroupNodes(e.ctx, &protos.NodeGroupNodesRequest{Id: "app"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range nodes.Instances {
		if in.Id == r.ProviderID {
			return in.Status.InstanceState
		}
	}
	t.Fatalf("%s not reported", name)
	return 0
}

// VM still running, node NotReady: the replacement is created first, the old instance deleted only once the
// replacement is Ready, and the target size never changes.
func TestRepairCreatesFirstWhenVMRuns(t *testing.T) {
	e := newEnv(t)
	e.p.Config.NodeGroups[0].MinSize, e.p.Config.NodeGroups[0].MaxSize = 1, 1 // min == max: cluster-autoscaler alone is stuck here
	old, _ := e.runningInstance(t)
	e.setVM(old.ID, cloud.PhaseRunning)
	e.setReady(t, old.Name, false)

	e.now = e.now.Add(9 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Running || len(e.p.Store.List("app")) != 1 {
		t.Fatalf("before notReadyAfter nothing happens: %+v", e.p.Store.List("app"))
	}

	e.now = e.now.Add(2 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Replacing {
		t.Fatalf("old instance: want Replacing, got %s", e.phase(old.Name))
	}
	nr := e.replacementOf(t, old.Name)
	if nr.Phase != state.Creating || len(e.cloud.deletes) != 0 {
		t.Fatalf("create first: replacement %s, deletes %v", nr.Phase, e.cloud.deletes)
	}
	if e.targetSize(t) != 1 {
		t.Fatalf("target size must stay 1 during the repair, got %d", e.targetSize(t))
	}
	if st := e.reportedState(t, old.Name); st != protos.InstanceStatus_instanceDeleting {
		t.Fatalf("a Replacing instance is reported as deleting, got %v", st)
	}

	e.now = e.now.Add(4 * time.Minute)
	e.register(t, nr.Name, true)
	e.p.ReconcileOnce(e.ctx) // replacement -> Running (old is reconciled first and still waits)
	e.p.ReconcileOnce(e.ctx) // old sees a Running replacement -> deleted
	if len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != old.ID {
		t.Fatalf("the old VM must be deleted once the replacement is Ready, deletes %v", e.cloud.deletes)
	}
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != "gone" {
		t.Fatalf("old record must be gone, got %s", e.phase(old.Name))
	}
	if _, err := e.kube.CoreV1().Nodes().Get(e.ctx, old.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("old Node object must be deleted")
	}
	if got, _ := e.p.Store.Get(nr.Name); got.Phase != state.Running || got.Replaces != "" || e.targetSize(t) != 1 {
		t.Fatalf("replacement %+v, target %d", got, e.targetSize(t))
	}
}

// VM gone, stopped or in error: the old instance is deleted while the replacement is created.
func TestRepairDeletesAtOnceWhenVMIsDead(t *testing.T) {
	for _, tc := range []struct {
		name string
		kill func(e *env, id string)
	}{
		{"stopped", func(e *env, id string) { e.setVM(id, cloud.PhaseStopped) }},
		{"error", func(e *env, id string) { e.setVM(id, cloud.PhaseError) }},
		{"deleted on the portal", func(e *env, id string) {
			e.cloud.mu.Lock()
			delete(e.cloud.vms, id)
			e.cloud.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.atMinSize(1)
			old, _ := e.runningInstance(t)
			tc.kill(e, old.ID)
			e.setReady(t, old.Name, false)

			e.now = e.now.Add(time.Minute)
			e.p.ReconcileOnce(e.ctx)
			if e.phase(old.Name) != state.Running {
				t.Fatalf("before vmGoneAfter nothing happens, got %s", e.phase(old.Name))
			}

			e.now = e.now.Add(90 * time.Second)
			e.p.ReconcileOnce(e.ctx)
			if p := e.phase(old.Name); p != state.Deleting && p != "gone" {
				t.Fatalf("old instance: want Deleting, got %s", p)
			}
			if len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != old.ID {
				t.Fatalf("delete must be issued at once, deletes %v", e.cloud.deletes)
			}
			if nr := e.replacementOf(t, old.Name); nr.Phase != state.Creating || e.targetSize(t) != 1 {
				t.Fatalf("replacement %s, target %d", nr.Phase, e.targetSize(t))
			}
			e.p.ReconcileOnce(e.ctx)
			if _, err := e.kube.CoreV1().Nodes().Get(e.ctx, old.Name, metav1.GetOptions{}); err == nil {
				t.Fatal("old Node object must be deleted once the VM is gone")
			}
		})
	}
}

// The old node recovers while its replacement is still being created: it is kept and the replacement is
// cancelled, so the target size never goes above what it was (review: maxSize + 1).
func TestRepairKeepsRecoveredNode(t *testing.T) {
	e := newEnv(t)
	e.p.Config.NodeGroups[0].MinSize, e.p.Config.NodeGroups[0].MaxSize = 1, 1
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	nr := e.replacementOf(t, old.Name)

	e.setReady(t, old.Name, true)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Running {
		t.Fatalf("recovered node must be kept, got %s", e.phase(old.Name))
	}
	if p := e.phase(nr.Name); p != state.Deleting && p != "gone" {
		t.Fatalf("the replacement must be cancelled, got %s", p)
	}
	if len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != nr.ID {
		t.Fatalf("only the replacement VM is deleted, deletes %v", e.cloud.deletes)
	}
	if ts := e.targetSize(t); ts != 1 {
		t.Fatalf("target size = %d, must not exceed maxSize 1", ts)
	}
}

// The same for a node that lost the group label: it is still found, by name and providerID.
func TestRepairKeepsRecoveredNodeWithoutLabel(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, node := e.runningInstance(t)
	node, _ = e.kube.CoreV1().Nodes().Get(e.ctx, node.Name, metav1.GetOptions{})
	node.Labels = map[string]string{}
	_, _ = e.kube.CoreV1().Nodes().Update(e.ctx, node, metav1.UpdateOptions{})
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	nr := e.replacementOf(t, old.Name)
	e.setReady(t, old.Name, true)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Running || len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != nr.ID {
		t.Fatalf("old %s, deletes %v", e.phase(old.Name), e.cloud.deletes)
	}
}

// Ready=False then Unknown moves lastTransitionTime; the repair timer must keep counting from the first
// NotReady (review: timer restarts).
func TestRepairTimerSurvivesFalseToUnknown(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	e.setReadyStatus(t, old.Name, corev1.ConditionFalse)
	e.p.ReconcileOnce(e.ctx)
	e.now = e.now.Add(8 * time.Minute)
	e.setReadyStatus(t, old.Name, corev1.ConditionUnknown) // lastTransitionTime = T+8m
	e.p.ReconcileOnce(e.ctx)
	e.now = e.now.Add(3 * time.Minute) // T+11m: 11m since the first NotReady, 3m since the last transition
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Replacing {
		t.Fatalf("want Replacing at T+11m, got %s", e.phase(old.Name))
	}
}

// cluster-autoscaler cancelling a scale up must not pick a repair's replacement (review: DecreaseTargetSize).
func TestDecreaseTargetSizeSparesReplacement(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	nr := e.replacementOf(t, old.Name)
	if _, err := e.p.NodeGroupDecreaseTargetSize(e.ctx, &protos.NodeGroupDecreaseTargetSizeRequest{Id: "app", Delta: -1}); err == nil {
		t.Fatal("the only Creating instance is a replacement: the decrease must be refused")
	}
	// a real scale up next to it: that one is cancelled, the replacement stays
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.NodeGroupDecreaseTargetSize(e.ctx, &protos.NodeGroupDecreaseTargetSizeRequest{Id: "app", Delta: -1}); err != nil {
		t.Fatal(err)
	}
	if e.phase(nr.Name) != state.Creating {
		t.Fatalf("the replacement must survive, got %s", e.phase(nr.Name))
	}
}

// No cloud call for a NotReady node before any threshold can be reached (review: wasted Get calls).
func TestRepairNoCloudCallBeforeThreshold(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	gets := e.cloud.gets
	e.now = e.now.Add(time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if e.cloud.gets != gets {
		t.Fatalf("no Driver.Get before vmGoneAfter, got %d calls", e.cloud.gets-gets)
	}
}

// A lost node whose replacement could not be created is retried after createRetryAfter, without a second
// nodeLostAfter wait (review: NodeMissingSince cleared).
func TestRepairLostNodeRetryKeepsMissingSince(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, node := e.runningInstance(t)
	_ = e.kube.CoreV1().Nodes().Delete(e.ctx, node.Name, metav1.DeleteOptions{})
	e.p.ReconcileOnce(e.ctx)
	e.now = e.now.Add(nodeLostAfter + time.Minute)
	e.cloud.failNext = errors.New("quota exceeded")
	e.p.ReconcileOnce(e.ctx)
	if got, _ := e.p.Store.Get(old.Name); got.Phase != state.Running || got.NodeMissingSince == nil {
		t.Fatalf("want Running with nodeMissingSince kept: %+v", got)
	}
	e.now = e.now.Add(createRetryAfter + time.Second)
	e.p.ReconcileOnce(e.ctx)
	if p := e.phase(old.Name); p != state.Deleting && p != "gone" {
		t.Fatalf("retry right after createRetryAfter, got %s", p)
	}
}

// The replacement never becomes a node: the old instance is kept and not repaired again before retryAfter.
func TestRepairReplacementFails(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	nr := e.replacementOf(t, old.Name)

	e.now = e.now.Add(21 * time.Minute) // past maxProvisionTime
	e.p.ReconcileOnce(e.ctx)
	e.p.ReconcileOnce(e.ctx)
	if got, _ := e.p.Store.Get(nr.Name); got.Phase != state.Failed {
		t.Fatalf("replacement: want Failed, got %s", got.Phase)
	}
	got, _ := e.p.Store.Get(old.Name)
	if got.Phase != state.Running || got.RepairAfter == nil || len(e.cloud.deletes) != 0 {
		t.Fatalf("old instance must be kept with a retry time: %+v, deletes %v", got, e.cloud.deletes)
	}

	// cluster-autoscaler deletes the failed replacement
	if _, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{ProviderID: nr.ProviderID}}}); err != nil {
		t.Fatal(err)
	}
	e.p.ReconcileOnce(e.ctx)
	n := len(e.p.Store.List("app"))
	e.now = e.now.Add(10 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if len(e.p.Store.List("app")) != n {
		t.Fatalf("no new repair before retryAfter: %+v", e.p.Store.List("app"))
	}
	e.now = e.now.Add(21 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Replacing {
		t.Fatalf("after retryAfter the repair runs again, got %s", e.phase(old.Name))
	}
}

// A create error (quota, API down) puts the instance back to Running and retries a few minutes later.
func TestRepairCreateErrorRetriesLater(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.cloud.failNext = errors.New("quota exceeded")
	e.p.ReconcileOnce(e.ctx)
	got, _ := e.p.Store.Get(old.Name)
	if got.Phase != state.Running || got.RepairAfter == nil || len(e.p.Store.List("app")) != 1 {
		t.Fatalf("want Running with a retry time and no leaked record: %+v", e.p.Store.List("app"))
	}
	e.now = e.now.Add(createRetryAfter + time.Second)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Replacing {
		t.Fatalf("retry after createRetryAfter, got %s", e.phase(old.Name))
	}
}

// A crash between marking Replacing and creating the replacement leaves a dangling link: back to Running,
// retried after createRetryAfter (not every pass).
func TestRepairDanglingReplacementIsReset(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	_ = e.p.Store.Update(e.ctx, old.Name, func(x *state.Record) bool {
		x.Phase, x.ReplacedBy = state.Replacing, "dev-app-never-created"
		return true
	})
	e.p.ReconcileOnce(e.ctx)
	got, _ := e.p.Store.Get(old.Name)
	if got.Phase != state.Running || got.ReplacedBy != "" || got.RepairAfter == nil || !got.RepairAfter.Equal(e.now.Add(createRetryAfter)) {
		t.Fatalf("want Running retried after createRetryAfter, got %+v", got)
	}
}

// One repair per group at a time.
func TestRepairOneAtATime(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(2)
	e.p.Config.Repair.MaxUnhealthyPercent = 100
	a, _ := e.runningInstance(t)
	b, _ := e.runningInstance(t)
	e.setReady(t, a.Name, false)
	e.setReady(t, b.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	replacing := 0
	for _, r := range e.p.Store.List("app") {
		if r.Phase == state.Replacing {
			replacing++
		}
	}
	if replacing != 1 {
		t.Fatalf("want exactly one repair in progress, got %d: %+v", replacing, e.p.Store.List("app"))
	}
}

// Circuit breaker: too many unhealthy nodes in the group, or in the whole cluster, means no repair.
func TestRepairCircuitBreaker(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		e := newEnv(t)
		e.atMinSize(5)
		var recs []state.Record
		for i := 0; i < 5; i++ {
			r, _ := e.runningInstance(t)
			recs = append(recs, r)
		}
		e.setReady(t, recs[0].Name, false)
		e.setReady(t, recs[1].Name, false) // 2 of 5 = 40% > 20%
		e.now = e.now.Add(11 * time.Minute)
		e.p.ReconcileOnce(e.ctx)
		if len(e.p.Store.List("app")) != 5 {
			t.Fatalf("breaker must block the repair: %+v", e.p.Store.List("app"))
		}
	})
	t.Run("cluster", func(t *testing.T) {
		e := newEnv(t)
		e.atMinSize(1)
		old, _ := e.runningInstance(t)
		for i := 0; i < 3; i++ { // other nodes of the cluster, two of them NotReady
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rke-%d", i)},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			if i < 2 {
				n.Status.Conditions[0].Status = corev1.ConditionUnknown
			}
			_, _ = e.kube.CoreV1().Nodes().Create(e.ctx, n, metav1.CreateOptions{})
		}
		e.setReady(t, old.Name, false) // 3 of 4 nodes not Ready
		e.now = e.now.Add(11 * time.Minute)
		e.p.ReconcileOnce(e.ctx)
		if e.phase(old.Name) != state.Running || len(e.p.Store.List("app")) != 1 {
			t.Fatalf("breaker must block the repair: %+v", e.p.Store.List("app"))
		}
	})
	t.Run("cluster check ignores the group override", func(t *testing.T) {
		e := newEnv(t)
		e.atMinSize(1)
		e.p.Config.NodeGroups[0].Repair = &config.Repair{MaxUnhealthyPercent: 100}
		old, _ := e.runningInstance(t)
		for i := 0; i < 3; i++ {
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("rke-%d", i)},
				Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionUnknown}}}}
			_, _ = e.kube.CoreV1().Nodes().Create(e.ctx, n, metav1.CreateOptions{})
		}
		e.setReady(t, old.Name, false) // 4 of 4 nodes not Ready
		e.now = e.now.Add(11 * time.Minute)
		e.p.ReconcileOnce(e.ctx)
		if e.phase(old.Name) != state.Running {
			t.Fatalf("cluster breaker must still block, got %s", e.phase(old.Name))
		}
	})
	t.Run("single node always repairable", func(t *testing.T) {
		e := newEnv(t)
		e.atMinSize(1)
		old, _ := e.runningInstance(t)
		e.setReady(t, old.Name, false) // 1 of 1 = 100%, but one node is always allowed
		e.now = e.now.Add(11 * time.Minute)
		e.p.ReconcileOnce(e.ctx)
		if e.phase(old.Name) != state.Replacing {
			t.Fatalf("got %s", e.phase(old.Name))
		}
	})
}

// Per group override: repair disabled for one group leaves its NotReady nodes alone.
func TestRepairDisabledForGroup(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	off := false
	e.p.Config.NodeGroups[0].Repair = &config.Repair{Enabled: &off}
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(time.Hour)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(old.Name) != state.Running || len(e.p.Store.List("app")) != 1 {
		t.Fatalf("repair disabled: %+v", e.p.Store.List("app"))
	}
}

// cluster-autoscaler deleting the old node during its repair (scale-down-unready-time) must not create a
// second replacement.
func TestRepairOldNodeDeletedByAutoscaler(t *testing.T) {
	e := newEnv(t)
	e.atMinSize(1)
	old, _ := e.runningInstance(t)
	e.setReady(t, old.Name, false)
	e.now = e.now.Add(11 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if _, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{ProviderID: old.ProviderID}}}); err != nil {
		t.Fatalf("deleting a Replacing instance is allowed (outside the target size): %v", err)
	}
	e.p.ReconcileOnce(e.ctx)
	e.p.ReconcileOnce(e.ctx)
	if n := len(e.p.Store.List("app")); n != 1 || e.targetSize(t) != 1 {
		t.Fatalf("want only the replacement left: %+v", e.p.Store.List("app"))
	}
}

// Above minSize a broken node is only removed: no replacement VM (the group may be about to scale down anyway;
// pods that need room become Pending and cluster-autoscaler scales up without backoff).
func TestRepairAboveMinSizeOnlyRemoves(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after time.Duration
		kill  func(e *env, r state.Record, n *corev1.Node)
	}{
		{"VM stopped", 3 * time.Minute, func(e *env, r state.Record, _ *corev1.Node) {
			e.setVM(r.ID, cloud.PhaseStopped)
			e.setReady(e.t, r.Name, false)
		}},
		{"kubelet dead, VM runs", 11 * time.Minute, func(e *env, r state.Record, _ *corev1.Node) {
			e.setVM(r.ID, cloud.PhaseRunning)
			e.setReady(e.t, r.Name, false)
		}},
		{"node object deleted", nodeLostAfter + time.Minute, func(e *env, _ state.Record, n *corev1.Node) {
			_ = e.kube.CoreV1().Nodes().Delete(e.ctx, n.Name, metav1.DeleteOptions{})
			e.p.ReconcileOnce(e.ctx) // records nodeMissingSince
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t) // minSize 0
			old, node := e.runningInstance(t)
			tc.kill(e, old, node)
			e.now = e.now.Add(tc.after)
			e.p.ReconcileOnce(e.ctx)
			if p := e.phase(old.Name); p != state.Deleting && p != "gone" {
				t.Fatalf("the broken instance must be removed, got %s", p)
			}
			if n := len(e.p.Store.List("app")); n > 1 {
				t.Fatalf("no replacement above minSize: %+v", e.p.Store.List("app"))
			}
			if len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != old.ID {
				t.Fatalf("its VM must be deleted, deletes %v", e.cloud.deletes)
			}
			if ts := e.targetSize(t); ts != 0 {
				t.Fatalf("target size = %d, want 0", ts)
			}
		})
	}
}

// Two nodes, minSize 1: the first broken node is removed (2 > 1), the second one, now at minSize, is replaced.
func TestRepairRemovesAboveMinThenReplacesAtMin(t *testing.T) {
	e := newEnv(t)
	e.p.Config.NodeGroups[0].MinSize = 1
	a, _ := e.runningInstance(t)
	b, _ := e.runningInstance(t)
	e.setVM(a.ID, cloud.PhaseStopped)
	e.setReady(t, a.Name, false)
	e.now = e.now.Add(3 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	e.p.ReconcileOnce(e.ctx)
	if e.phase(a.Name) != "gone" || len(e.p.Store.List("app")) != 1 {
		t.Fatalf("first broken node removed without replacement: %+v", e.p.Store.List("app"))
	}
	e.setVM(b.ID, cloud.PhaseStopped)
	e.setReady(t, b.Name, false)
	e.now = e.now.Add(3 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if nr := e.replacementOf(t, b.Name); nr.Phase != state.Creating || e.targetSize(t) != 1 {
		t.Fatalf("at minSize the node is replaced: %+v target %d", nr, e.targetSize(t))
	}
}

// A Node deleted by the Deleting step is not deleted again by the orphan pass of the same reconcile, and a
// normal registration does not log an empty "replaces".
func TestNoDuplicateOrphanDeleteAndCleanRegisterLog(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.p.Log = slog.New(slog.NewTextHandler(&buf, nil))
	old, _ := e.runningInstance(t)
	if strings.Contains(buf.String(), "replaces=") {
		t.Fatalf("a plain registration must not log replaces: %s", buf.String())
	}
	if _, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{ProviderID: old.ProviderID}}}); err != nil {
		t.Fatal(err)
	}
	e.p.ReconcileOnce(e.ctx)
	if !strings.Contains(buf.String(), "instance removed") {
		t.Fatalf("setup: instance not removed: %s", buf.String())
	}
	if strings.Contains(buf.String(), "deleting orphan node") {
		t.Fatalf("the node was already deleted by the Deleting step: %s", buf.String())
	}
}
