package provider

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

func (e *env) setFlavor(f string) {
	e.p.Config.NodeGroups[0].VNGCloud = &config.VNGCloudServer{ImageID: "img-1", FlavorID: f}
}

// addPod puts a pod on a node; kind is the controller kind ("" for none, "mirror" for a static pod).
func (e *env) addPod(t *testing.T, node, name, kind string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	switch kind {
	case "":
	case "mirror":
		pod.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "x"}
	default:
		yes := true
		pod.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: "owner", Controller: &yes}}
	}
	if _, err := e.kube.CoreV1().Pods("default").Create(e.ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// evictions makes the API server answer evictions: with err, or by deleting the pod. It returns the names
// of the evicted pods.
func (e *env) evictions(err error) *[]string {
	var got []string
	e.kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "eviction" {
			return false, nil, nil
		}
		ev := a.(k8stesting.CreateAction).GetObject().(*policyv1.Eviction)
		if err != nil {
			return true, nil, err
		}
		got = append(got, ev.Name)
		return true, nil, e.kube.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, ev.Namespace, ev.Name)
	})
	return &got
}

func (e *env) newest(t *testing.T) state.Record {
	t.Helper()
	recs := e.p.Store.List("app")
	return recs[len(recs)-1]
}

func TestRolloutReplacesOutdatedNodesOneAtATime(t *testing.T) {
	e := newEnv(t)
	e.setFlavor("2x8")
	a, _ := e.runningInstance(t)
	e.now = e.now.Add(time.Minute)
	b, _ := e.runningInstance(t)
	e.addPod(t, a.Name, "web", "ReplicaSet")
	e.addPod(t, a.Name, "canal", "DaemonSet")
	e.addPod(t, a.Name, "static", "mirror")
	evicted := e.evictions(nil)

	e.p.ReconcileOnce(e.ctx)
	if r, _ := e.p.Store.Get(a.Name); r.Phase != state.Running {
		t.Fatalf("nothing changed, nothing to roll out: %s", r.Phase)
	}

	e.setFlavor("2x16")
	e.p.ReconcileOnce(e.ctx)
	old, _ := e.p.Store.Get(a.Name)
	if old.Phase != state.Replacing || !old.Rollout {
		t.Fatalf("the oldest outdated node must be replaced first: %+v", old)
	}
	if r, _ := e.p.Store.Get(b.Name); r.Phase != state.Running {
		t.Fatalf("one node at a time: %s is %s", b.Name, r.Phase)
	}
	if got := e.targetSize(t); got != 2 {
		t.Fatalf("target size = %d, want 2", got)
	}
	repl := e.newest(t)
	if repl.Replaces != a.Name || repl.Spec != e.p.Config.NodeGroups[0].SpecHash() {
		t.Fatalf("replacement: %+v", repl)
	}

	// replacement not Ready yet: the old node keeps serving
	e.p.ReconcileOnce(e.ctx)
	if n, _ := e.kube.CoreV1().Nodes().Get(e.ctx, a.Name, metav1.GetOptions{}); n.Spec.Unschedulable {
		t.Fatal("the old node must not be cordoned before its replacement is Ready")
	}
	if r, _ := e.p.Store.Get(repl.Name); r.Phase != state.Creating {
		t.Fatalf("a Ready old node must not cancel a rollout replacement: %s", r.Phase)
	}

	e.register(t, repl.Name, true)
	e.p.ReconcileOnce(e.ctx) // replacement Running
	e.p.ReconcileOnce(e.ctx) // cordon + evict
	n, _ := e.kube.CoreV1().Nodes().Get(e.ctx, a.Name, metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Fatal("old node not cordoned")
	}
	if len(*evicted) != 1 || (*evicted)[0] != "web" {
		t.Fatalf("only the ReplicaSet pod is evicted, got %v", *evicted)
	}
	e.p.ReconcileOnce(e.ctx) // drained -> delete -> next node
	if r, _ := e.p.Store.Get(a.Name); r.Phase != state.Deleting {
		t.Fatalf("drained old instance must be deleted: %s", r.Phase)
	}
	e.p.ReconcileOnce(e.ctx)
	if _, ok := e.p.Store.Get(a.Name); ok {
		t.Fatal("drained old instance must be deleted")
	}
	if r, _ := e.p.Store.Get(b.Name); r.Phase != state.Replacing || !r.Rollout {
		t.Fatalf("second outdated node is next: %+v", r)
	}
	if got := e.targetSize(t); got != 2 {
		t.Fatalf("target size = %d, want 2", got)
	}
}

func TestRolloutWaitsForPodDisruptionBudget(t *testing.T) {
	e := newEnv(t)
	e.setFlavor("2x8")
	a, _ := e.runningInstance(t)
	e.addPod(t, a.Name, "db", "StatefulSet")
	e.evictions(apierrors.NewTooManyRequests("pdb", 10))

	e.setFlavor("2x16")
	e.p.ReconcileOnce(e.ctx)
	repl := e.newest(t)
	e.register(t, repl.Name, true)
	for i := 0; i < 3; i++ {
		e.now = e.now.Add(time.Hour)
		e.p.ReconcileOnce(e.ctx)
	}
	if r, _ := e.p.Store.Get(a.Name); r.Phase != state.Replacing || r.DrainStartedAt == nil {
		t.Fatalf("a PodDisruptionBudget must hold the old node: %+v", r)
	}
	if len(e.cloud.deletes) != 0 {
		t.Fatalf("no VM may be deleted: %v", e.cloud.deletes)
	}
}

func TestRolloutKeepsPodsWithoutController(t *testing.T) {
	e := newEnv(t)
	e.setFlavor("2x8")
	a, _ := e.runningInstance(t)
	e.addPod(t, a.Name, "debug", "")
	evicted := e.evictions(nil)

	e.setFlavor("2x16")
	e.p.ReconcileOnce(e.ctx)
	e.register(t, e.newest(t).Name, true)
	e.p.ReconcileOnce(e.ctx)
	e.p.ReconcileOnce(e.ctx)
	if len(*evicted) != 0 {
		t.Fatalf("a pod without controller must not be evicted: %v", *evicted)
	}
	if r, _ := e.p.Store.Get(a.Name); r.Phase != state.Replacing {
		t.Fatalf("old node must stay: %s", r.Phase)
	}
	// deleted by hand: the drain completes
	_ = e.kube.CoreV1().Pods("default").Delete(e.ctx, "debug", metav1.DeleteOptions{})
	e.p.ReconcileOnce(e.ctx)
	e.p.ReconcileOnce(e.ctx)
	if _, ok := e.p.Store.Get(a.Name); ok {
		t.Fatal("old instance must be gone once drained")
	}
}

func TestRolloutStopsWhenReplacementFails(t *testing.T) {
	e := newEnv(t)
	e.setFlavor("2x8")
	a, _ := e.runningInstance(t)
	e.now = e.now.Add(time.Minute)
	b, _ := e.runningInstance(t)

	e.setFlavor("2x16")
	e.p.ReconcileOnce(e.ctx)
	repl := e.newest(t)
	e.now = e.now.Add(21 * time.Minute) // past maxProvisionTime, never registered
	e.p.ReconcileOnce(e.ctx)
	e.p.ReconcileOnce(e.ctx)
	old, _ := e.p.Store.Get(a.Name)
	if old.Phase != state.Running || old.Rollout || old.RepairAfter == nil {
		t.Fatalf("old node must be kept with a retry delay: %+v", old)
	}
	if r, _ := e.p.Store.Get(repl.Name); r.Phase != state.Failed {
		t.Fatalf("replacement: %s", r.Phase)
	}
	if r, _ := e.p.Store.Get(b.Name); r.Phase != state.Running {
		t.Fatalf("a failed step must stop the rollout: %s is %s", b.Name, r.Phase)
	}
}

func TestRolloutWaitsForUnhealthyGroup(t *testing.T) {
	e := newEnv(t)
	e.setFlavor("2x8")
	e.runningInstance(t)
	_, nb := e.runningInstance(t)
	e.setReady(t, nb.Name, false)

	e.setFlavor("2x16")
	e.p.ReconcileOnce(e.ctx)
	for _, r := range e.p.Store.List("app") {
		if r.Phase != state.Running {
			t.Fatalf("no rollout while a node of the group is NotReady: %s is %s", r.Name, r.Phase)
		}
	}
}

func TestRolloutDisabled(t *testing.T) {
	e := newEnv(t)
	off := false
	e.p.Config.NodeGroups[0].Rollout = &config.Rollout{Enabled: &off}
	e.setFlavor("2x8")
	a, _ := e.runningInstance(t)
	e.setFlavor("2x16")
	e.p.ReconcileOnce(e.ctx)
	if r, _ := e.p.Store.Get(a.Name); r.Phase != state.Running {
		t.Fatalf("rollout disabled for the group: %s", r.Phase)
	}
}

func TestAdoptSpecs(t *testing.T) {
	e := newEnv(t)
	e.setFlavor("2x8")
	a, _ := e.runningInstance(t)
	// an instance recorded by a version without rollouts
	if err := e.p.Store.Update(e.ctx, a.Name, func(x *state.Record) bool { x.Spec = ""; return true }); err != nil {
		t.Fatal(err)
	}
	e.setFlavor("2x16")
	if err := e.p.AdoptSpecs(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.p.ReconcileOnce(e.ctx)
	r, _ := e.p.Store.Get(a.Name)
	if r.Phase != state.Running || r.Spec != e.p.Config.NodeGroups[0].SpecHash() {
		t.Fatalf("adopted instance must not be rolled: %+v", r)
	}
}
