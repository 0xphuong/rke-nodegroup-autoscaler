package provider

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

// fakeCloud is an in-memory cloud.
type fakeCloud struct {
	mu       sync.Mutex
	next     int
	vms      map[string]*cloud.Instance
	userData map[string]string
	deletes  []string
	failNext error
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{vms: map[string]*cloud.Instance{}, userData: map[string]string{}}
}

func (f *fakeCloud) ProviderIDPrefix() string { return "fake://" }

func (f *fakeCloud) Create(_ context.Context, _ config.NodeGroup, req cloud.CreateRequest) (cloud.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return cloud.Instance{}, err
	}
	f.next++
	in := &cloud.Instance{ID: fmt.Sprintf("vm-%d", f.next), Name: req.Name, Phase: cloud.PhaseCreating}
	f.vms[in.ID] = in
	f.userData[req.Name] = req.UserData
	return *in, nil
}

func (f *fakeCloud) Get(_ context.Context, id string) (cloud.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.vms[id]
	if !ok {
		return cloud.Instance{}, cloud.ErrNotFound
	}
	return *in, nil
}

func (f *fakeCloud) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	delete(f.vms, id) // the fake deletes instantly
	return nil
}

type env struct {
	p     *Provider
	cloud *fakeCloud
	kube  *fake.Clientset
	now   time.Time
	ctx   context.Context
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg := &config.Config{
		ClusterName:      "dev",
		MaxProvisionTime: metav1.Duration{Duration: 20 * time.Minute},
		Bootstrap: config.Bootstrap{
			Endpoints: []string{"https://10.0.0.11:31443"},
			TokenTTL:  metav1.Duration{Duration: 15 * time.Minute},
		},
		NodeGroups: []config.NodeGroup{{
			Name: "app", MinSize: 0, MaxSize: 3, NamePrefix: "dev-app",
			Labels: map[string]string{"debug": "true"}, Taints: []string{"debug=true:NoSchedule"},
			Resources: config.Resources{CPU: resource.MustParse("4"), Memory: resource.MustParse("8Gi"), Pods: resource.MustParse("110")},
		}},
	}
	e := &env{cloud: newFakeCloud(), kube: fake.NewSimpleClientset(), now: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC), ctx: context.Background()}
	store := state.New(e.kube, "kube-system", "state")
	if err := store.Load(e.ctx); err != nil {
		t.Fatal(err)
	}
	n := 0
	e.p = &Provider{
		Config: cfg, Driver: e.cloud, Store: store, Kube: e.kube, BootCA: "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return e.now },
		NameRand: func() string { n++; return fmt.Sprintf("n%d", n) },
	}
	return e
}

func (e *env) targetSize(t *testing.T) int32 {
	t.Helper()
	r, err := e.p.NodeGroupTargetSize(e.ctx, &protos.NodeGroupTargetSizeRequest{Id: "app"})
	if err != nil {
		t.Fatal(err)
	}
	return r.TargetSize
}

// register simulates kubelet registering the node of a record.
func (e *env) register(t *testing.T, name string, ready bool) *corev1.Node {
	t.Helper()
	rec, _ := e.p.Store.Get(name)
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{config.GroupLabel: rec.Group}},
		Spec:       corev1.NodeSpec{ProviderID: rec.ProviderID},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}},
	}
	if _, err := e.kube.CoreV1().Nodes().Create(e.ctx, n, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScaleUpRegisterScaleDown(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 2}); err != nil {
		t.Fatal(err)
	}
	if got := e.targetSize(t); got != 2 {
		t.Fatalf("target size = %d, want 2", got)
	}
	recs := e.p.Store.List("app")
	if len(recs) != 2 || recs[0].ID == "" || !strings.HasPrefix(recs[0].ProviderID, "fake://vm-") || recs[0].TokenHash == "" {
		t.Fatalf("unexpected records: %+v", recs)
	}
	ud := e.cloud.userData[recs[0].Name]
	if !strings.Contains(ud, "Authorization: Bearer ") || !strings.Contains(ud, "https://10.0.0.11:31443") {
		t.Errorf("user data lacks token or endpoint:\n%s", ud)
	}
	if strings.Contains(ud, "PRIVATE KEY") {
		t.Error("user data must never carry key material")
	}

	// instances are reported as creating
	nodes, _ := e.p.NodeGroupNodes(e.ctx, &protos.NodeGroupNodesRequest{Id: "app"})
	if len(nodes.Instances) != 2 || nodes.Instances[0].Status.InstanceState != protos.InstanceStatus_instanceCreating {
		t.Fatalf("instances: %+v", nodes.Instances)
	}

	// node registers Ready -> Running, token burnt
	node := e.register(t, recs[0].Name, true)
	e.p.ReconcileOnce(e.ctx)
	r0, _ := e.p.Store.Get(recs[0].Name)
	if r0.Phase != state.Running || r0.TokenHash != "" {
		t.Fatalf("after register: %+v", r0)
	}

	// cluster-autoscaler maps the node to its group
	ng, _ := e.p.NodeGroupForNode(e.ctx, &protos.NodeGroupForNodeRequest{Node: &protos.ExternalGrpcNode{Name: node.Name, ProviderID: node.Spec.ProviderID}})
	if ng.NodeGroup.Id != "app" {
		t.Fatalf("NodeGroupForNode = %q", ng.NodeGroup.Id)
	}

	// scale down: VM first, Node object on the next reconcile
	if _, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{Name: node.Name, ProviderID: node.Spec.ProviderID}}}); err != nil {
		t.Fatal(err)
	}
	if len(e.cloud.deletes) != 1 || e.cloud.deletes[0] != r0.ID {
		t.Fatalf("cloud deletes = %v", e.cloud.deletes)
	}
	if got := e.targetSize(t); got != 1 {
		t.Fatalf("target size after delete = %d, want 1", got)
	}
	e.p.ReconcileOnce(e.ctx)
	if _, err := e.kube.CoreV1().Nodes().Get(e.ctx, node.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("Node object should be deleted once the VM is gone")
	}
	if _, ok := e.p.Store.Get(r0.Name); ok {
		t.Fatal("record should be removed")
	}
}

func TestUnmanagedNodesAreNeverInAGroup(t *testing.T) {
	e := newEnv(t)
	for _, n := range []*protos.ExternalGrpcNode{
		{Name: "cp-0"},
		{Name: "rke-worker-0", ProviderID: ""},
		{Name: "x", ProviderID: "fake://vm-999"},
		{Name: "y", ProviderID: "other://1", Labels: map[string]string{config.GroupLabel: "app"}},
	} {
		r, err := e.p.NodeGroupForNode(e.ctx, &protos.NodeGroupForNodeRequest{Node: n})
		if err != nil || r.NodeGroup.GetId() != "" {
			t.Errorf("node %s: group %q err %v, want none", n.Name, r.GetNodeGroup().GetId(), err)
		}
	}
	// and cannot be deleted through the provider
	_, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{Name: "rke-worker-0", ProviderID: "fake://vm-999"}}})
	if err == nil {
		t.Fatal("deleting a node the provider did not create must fail")
	}
	if len(e.cloud.deletes) != 0 {
		t.Fatal("no cloud delete may happen")
	}
}

func TestSizeLimits(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 4}); err == nil {
		t.Fatal("exceeding maxSize must fail")
	}
	if len(e.p.Store.List("")) != 0 {
		t.Fatal("nothing may be created when the request is refused")
	}
	e.p.Config.NodeGroups[0].MinSize = 1
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	r := e.p.Store.List("app")[0]
	e.register(t, r.Name, true)
	e.p.ReconcileOnce(e.ctx)
	_, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{Name: r.Name, ProviderID: r.ProviderID}}})
	if err == nil {
		t.Fatal("going below minSize must fail")
	}
}

func TestFailedCreateLeavesNothingBehind(t *testing.T) {
	e := newEnv(t)
	e.cloud.failNext = fmt.Errorf("quota exceeded")
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err == nil {
		t.Fatal("want error")
	}
	if n := len(e.p.Store.List("")); n != 0 {
		t.Fatalf("records left: %d", n)
	}
}

func TestProvisionTimeoutReportsFailure(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(21 * time.Minute)
	e.p.ReconcileOnce(e.ctx)
	r := e.p.Store.List("app")[0]
	if r.Phase != state.Failed {
		t.Fatalf("phase = %s, want Failed", r.Phase)
	}
	nodes, _ := e.p.NodeGroupNodes(e.ctx, &protos.NodeGroupNodesRequest{Id: "app"})
	if nodes.Instances[0].Status.ErrorInfo == nil {
		t.Fatal("failed instance must carry error info so cluster-autoscaler deletes it")
	}
	if got := e.targetSize(t); got != 0 {
		t.Fatalf("failed instances are outside the target size, got %d", got)
	}
	// cluster-autoscaler then deletes it
	if _, err := e.p.NodeGroupDeleteNodes(e.ctx, &protos.NodeGroupDeleteNodesRequest{Id: "app",
		Nodes: []*protos.ExternalGrpcNode{{ProviderID: r.ProviderID}}}); err != nil {
		t.Fatal(err)
	}
	e.p.ReconcileOnce(e.ctx)
	if len(e.p.Store.List("")) != 0 {
		t.Fatal("record should be gone")
	}
}

func TestDecreaseTargetSizeOnlyCancelsUnregistered(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 2}); err != nil {
		t.Fatal(err)
	}
	first := e.p.Store.List("app")[0]
	e.register(t, first.Name, true)
	e.p.ReconcileOnce(e.ctx)
	if _, err := e.p.NodeGroupDecreaseTargetSize(e.ctx, &protos.NodeGroupDecreaseTargetSizeRequest{Id: "app", Delta: -2}); err == nil {
		t.Fatal("cannot cancel a registered node")
	}
	if _, err := e.p.NodeGroupDecreaseTargetSize(e.ctx, &protos.NodeGroupDecreaseTargetSizeRequest{Id: "app", Delta: -1}); err != nil {
		t.Fatal(err)
	}
	if got := e.targetSize(t); got != 1 {
		t.Fatalf("target size = %d, want 1", got)
	}
	if r, _ := e.p.Store.Get(first.Name); r.Phase != state.Running {
		t.Fatal("the registered node must be untouched")
	}
}

func TestOrphanNodeOfVanishedVMIsDeleted(t *testing.T) {
	e := newEnv(t)
	orphan := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-app-lost", Labels: map[string]string{config.GroupLabel: "app"}},
		Spec:       corev1.NodeSpec{ProviderID: "fake://vm-gone"},
	}
	foreign := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "rke-worker-0"},
		Spec:       corev1.NodeSpec{ProviderID: "fake://vm-gone-too"},
	}
	for _, n := range []*corev1.Node{orphan, foreign} {
		if _, err := e.kube.CoreV1().Nodes().Create(e.ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.p.ReconcileOnce(e.ctx)
	if _, err := e.kube.CoreV1().Nodes().Get(e.ctx, orphan.Name, metav1.GetOptions{}); err == nil {
		t.Error("orphan node with the group label should be deleted")
	}
	if _, err := e.kube.CoreV1().Nodes().Get(e.ctx, foreign.Name, metav1.GetOptions{}); err != nil {
		t.Error("a node without the group label must never be deleted")
	}
}

func TestTemplateNodeInfo(t *testing.T) {
	e := newEnv(t)
	r, err := e.p.NodeGroupTemplateNodeInfo(e.ctx, &protos.NodeGroupTemplateNodeInfoRequest{Id: "app"})
	if err != nil {
		t.Fatal(err)
	}
	n := r.NodeInfo
	if n.Labels[config.GroupLabel] != "app" || n.Labels["debug"] != "true" {
		t.Errorf("labels = %v", n.Labels)
	}
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "debug" || n.Spec.Taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("taints = %v", n.Spec.Taints)
	}
	if n.Status.Allocatable.Cpu().String() != "4" || n.Status.Allocatable.Memory().String() != "8Gi" {
		t.Errorf("allocatable = %v", n.Status.Allocatable)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.NodeGroupIncreaseSize(e.ctx, &protos.NodeGroupIncreaseSizeRequest{Id: "app", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	again := state.New(e.kube, "kube-system", "state")
	if err := again.Load(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(again.List("app")) != 1 {
		t.Fatal("records must be persisted in the ConfigMap")
	}
}
