package provider

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

// Bug 1: a Failed instance is outside the target size, so deleting it never lowers the group; it must not be
// refused by the minSize guard (with enforce-node-group-min-size the group sits at minSize all the time).
func TestDeleteFailedInstanceAtMinSize(t *testing.T) {
	e := newEnv(t)
	e.p.Config.NodeGroups[0].MinSize = 1
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(21 * time.Minute) // never registered -> Failed
	e.p.ReconcileOnce(e.ctx)
	r := e.p.Store.List("app")[0]
	if r.Phase != state.Failed || e.targetSize(t) != 0 {
		t.Fatalf("setup: phase %s target %d", r.Phase, e.targetSize(t))
	}
	if _, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{ProviderID: r.ProviderID}}}); err != nil {
		t.Fatalf("deleting a Failed instance at minSize must succeed: %v", err)
	}
	if len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != r.ID {
		t.Fatalf("the failed VM must be deleted, cloud deletes = %v", e.cloud.deletes)
	}
}

// Bug 2: cancelling instances that are still being created must not take the group below minSize; otherwise
// min-size enforcement creates them again on the next loop and VMs churn.
func TestDecreaseTargetSizeRespectsMinSize(t *testing.T) {
	e := newEnv(t)
	e.p.Config.NodeGroups[0].MinSize = 2
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.NodeGroupDecreaseTargetSize(e.ctx, &protos.NodeGroupDecreaseTargetSizeRequest{Id: "app", Delta: -2}); err == nil {
		t.Fatal("going from 3 to 1 with minSize 2 must be refused")
	}
	if len(e.cloud.deletes) != 0 || e.targetSize(t) != 3 {
		t.Fatalf("a refused decrease must change nothing: deletes %v target %d", e.cloud.deletes, e.targetSize(t))
	}
	if _, err := e.p.NodeGroupDecreaseTargetSize(e.ctx, &protos.NodeGroupDecreaseTargetSizeRequest{Id: "app", Delta: -1}); err != nil {
		t.Fatalf("3 -> 2 with minSize 2 is allowed: %v", err)
	}
	if e.targetSize(t) != 2 {
		t.Fatalf("target size = %d, want 2", e.targetSize(t))
	}
}

// Bug 3: a Running instance whose Node object disappeared while the VM still exists must not count towards
// the target size forever; after a grace period it is failed so cluster-autoscaler replaces it.
func TestRunningInstanceWithLostNodeIsReplaced(t *testing.T) {
	e := newEnv(t)
	e.p.Config.NodeGroups[0].MinSize = 1
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	r := e.p.Store.List("app")[0]
	node := e.register(t, r.Name, true)
	e.p.ReconcileOnce(e.ctx)
	if got, _ := e.p.Store.Get(r.Name); got.Phase != state.Running {
		t.Fatalf("setup: phase %s", got.Phase)
	}

	// the Node object goes away (kubelet gone, or deleted by hand); the VM stays
	if err := e.kube.CoreV1().Nodes().Delete(e.ctx, node.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.p.ReconcileOnce(e.ctx)
	if got, _ := e.p.Store.Get(r.Name); got.Phase != state.Running {
		t.Fatalf("within the grace period the instance stays Running, got %s", got.Phase)
	}

	e.now = e.now.Add(nodeLostAfter + time.Minute)
	e.p.ReconcileOnce(e.ctx)
	got, _ := e.p.Store.Get(r.Name)
	if got.Phase != state.Failed {
		t.Fatalf("after the grace period the instance must be Failed, got %s", got.Phase)
	}
	if e.targetSize(t) != 0 {
		t.Fatalf("a lost node must not satisfy minSize, target size = %d", e.targetSize(t))
	}
	nodes, _ := e.p.NodeGroupNodes(e.ctx, &protos.NodeGroupNodesRequest{Id: "app"})
	if nodes.Instances[0].Status.ErrorInfo == nil {
		t.Fatal("the failed instance must carry error info so cluster-autoscaler deletes it")
	}
}

// A node that only flaps away briefly keeps its instance, and the missing marker is cleared.
func TestRunningInstanceNodeComesBack(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	r := e.p.Store.List("app")[0]
	node := e.register(t, r.Name, true)
	e.p.ReconcileOnce(e.ctx)
	_ = e.kube.CoreV1().Nodes().Delete(e.ctx, node.Name, metav1.DeleteOptions{})
	e.p.ReconcileOnce(e.ctx)
	e.now = e.now.Add(2 * time.Minute)
	e.register(t, r.Name, true) // kubelet re-registers
	e.p.ReconcileOnce(e.ctx)
	e.now = e.now.Add(nodeLostAfter + time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if got, _ := e.p.Store.Get(r.Name); got.Phase != state.Running {
		t.Fatalf("a node that came back must stay Running, got %s", got.Phase)
	}
}

// A Node that lost the group label (edited by hand) is still there: it is not "lost".
func TestRunningInstanceNodeWithoutLabelIsNotLost(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	r := e.p.Store.List("app")[0]
	node := e.register(t, r.Name, true)
	e.p.ReconcileOnce(e.ctx)
	node.Labels = map[string]string{}
	if _, err := e.kube.CoreV1().Nodes().Update(e.ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.p.ReconcileOnce(e.ctx)
	e.now = e.now.Add(nodeLostAfter + time.Minute)
	e.p.ReconcileOnce(e.ctx)
	if got, _ := e.p.Store.Get(r.Name); got.Phase != state.Running {
		t.Fatalf("a node that still exists must not be failed, got %s", got.Phase)
	}
}

// GPULabel answers with an empty label: no reserved key, no error log in cluster-autoscaler.
func TestGPULabelIsEmpty(t *testing.T) {
	e := newEnv(t)
	r, err := e.p.GPULabel(e.ctx, &protos.GPULabelRequest{})
	if err != nil || r.GetLabel() != "" {
		t.Fatalf("GPULabel = %q, %v; want empty label and no error", r.GetLabel(), err)
	}
}
