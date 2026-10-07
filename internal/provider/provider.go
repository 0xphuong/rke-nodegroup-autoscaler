// Package provider implements cluster-autoscaler's externalgrpc CloudProvider service on top of a cloud.Driver.
//
// Scale up:   IncreaseSize -> record + token -> create VM with user_data -> VM fetches its join script
//
//	-> kubelet registers with --provider-id -> reconcile marks it Running.
//
// Scale down: cluster-autoscaler taints and drains the node itself, then DeleteNodes -> mark Deleting
//
//	-> delete VM -> reconcile waits until the VM is gone, then deletes the Node object.
//
// The VM goes first so kubelet cannot re-register a Node that was just deleted.
package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/bootstrap"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

type Provider struct {
	protos.UnimplementedCloudProviderServer

	Config   *config.Config
	Driver   cloud.Driver
	Store    *state.Store
	Kube     kubernetes.Interface
	BootCA   string // PEM put into user_data so the VM can verify the bootstrap server
	Log      *slog.Logger
	Now      func() time.Time
	NameRand func() string

	// scaling operations on the same provider are serialized; they are rare and each is short
	mu sync.Mutex
}

// ---- node groups -------------------------------------------------------------------------------------------

func (p *Provider) NodeGroups(context.Context, *protos.NodeGroupsRequest) (*protos.NodeGroupsResponse, error) {
	out := &protos.NodeGroupsResponse{}
	for _, g := range p.Config.NodeGroups {
		out.NodeGroups = append(out.NodeGroups, pbGroup(g))
	}
	return out, nil
}

func (p *Provider) NodeGroupForNode(_ context.Context, req *protos.NodeGroupForNodeRequest) (*protos.NodeGroupForNodeResponse, error) {
	n := req.GetNode()
	// only nodes this provider created belong to a group; everything else (control plane, RKE-managed
	// workers, hand-joined nodes) must never be touched -> empty id
	if rec, ok := p.Store.ByProviderID(n.GetProviderID()); ok {
		if g, ok := p.Config.Group(rec.Group); ok {
			return &protos.NodeGroupForNodeResponse{NodeGroup: pbGroup(g)}, nil
		}
	}
	return &protos.NodeGroupForNodeResponse{NodeGroup: &protos.NodeGroup{}}, nil
}

func (p *Provider) NodeGroupTargetSize(_ context.Context, req *protos.NodeGroupTargetSizeRequest) (*protos.NodeGroupTargetSizeResponse, error) {
	if _, err := p.group(req.GetId()); err != nil {
		return nil, err
	}
	return &protos.NodeGroupTargetSizeResponse{TargetSize: int32(p.targetSize(req.GetId()))}, nil
}

// targetSize counts instances that are, or will become, nodes.
func (p *Provider) targetSize(group string) int {
	n := 0
	for _, r := range p.Store.List(group) {
		if r.Phase == state.Creating || r.Phase == state.Running {
			n++
		}
	}
	return n
}

// ---- scale up ----------------------------------------------------------------------------------------------

func (p *Provider) NodeGroupIncreaseSize(ctx context.Context, req *protos.NodeGroupIncreaseSizeRequest) (*protos.NodeGroupIncreaseSizeResponse, error) {
	g, err := p.group(req.GetId())
	if err != nil {
		return nil, err
	}
	delta := int(req.GetDelta())
	if delta <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be positive, got %d", delta)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if size := p.targetSize(g.Name); size+delta > g.MaxSize {
		return nil, status.Errorf(codes.InvalidArgument, "size %d + %d exceeds maxSize %d of %s", size, delta, g.MaxSize, g.Name)
	}

	// names are picked up front, unique within the batch and against existing records
	names := make([]string, 0, delta)
	taken := map[string]bool{}
	for len(names) < delta {
		name := g.NamePrefix + "-" + p.NameRand()
		if _, exists := p.Store.Get(name); exists || taken[name] {
			continue
		}
		taken[name] = true
		names = append(names, name)
	}
	var eg errgroup.Group
	for _, name := range names {
		eg.Go(func() error { return p.createOne(ctx, g, name) })
	}
	if err := eg.Wait(); err != nil {
		return nil, status.Errorf(codes.Unavailable, "scale up %s: %v", g.Name, err)
	}
	return &protos.NodeGroupIncreaseSizeResponse{}, nil
}

func (p *Provider) createOne(ctx context.Context, g config.NodeGroup, name string) error {
	token, hash, err := bootstrap.NewToken()
	if err != nil {
		return err
	}
	now := p.Now()
	// recorded before the cloud call: if we crash in between, reconcile finds a record without ID and drops it
	rec := state.Record{
		Name: name, Group: g.Name, Phase: state.Creating, CreatedAt: now,
		TokenHash: hash, TokenExpiry: now.Add(p.Config.Bootstrap.TokenTTL.Duration),
	}
	if err := p.Store.Put(ctx, rec); err != nil {
		return err
	}
	userData, err := bootstrap.UserData(bootstrap.UserDataParams{
		NodeName: name, Token: token, Endpoints: p.Config.Bootstrap.Endpoints, CACert: p.BootCA,
		PreJoin: p.Config.Bootstrap.PreJoinScript, Deadline: p.Config.Bootstrap.TokenTTL.Duration,
	})
	if err != nil {
		_ = p.Store.Remove(ctx, name)
		return err
	}
	inst, err := p.Driver.Create(ctx, g, cloud.CreateRequest{
		Name: name, UserData: userData,
		Tags: map[string]string{"rke-autoscaler-cluster": p.Config.ClusterName, "rke-autoscaler-nodegroup": g.Name},
	})
	if err != nil {
		_ = p.Store.Remove(ctx, name)
		return err
	}
	if err := p.Store.Update(ctx, name, func(r *state.Record) bool {
		r.ID, r.ProviderID = inst.ID, p.Driver.ProviderIDPrefix()+inst.ID
		return true
	}); err != nil {
		// the VM exists but we could not record its ID: delete it rather than leak an untracked VM
		_ = p.Driver.Delete(ctx, inst.ID)
		_ = p.Store.Remove(ctx, name)
		return fmt.Errorf("record instance %s: %w", inst.ID, err)
	}
	p.Log.Info("instance created", "group", g.Name, "node", name, "id", inst.ID)
	return nil
}

// ---- scale down --------------------------------------------------------------------------------------------

func (p *Provider) NodeGroupDeleteNodes(ctx context.Context, req *protos.NodeGroupDeleteNodesRequest) (*protos.NodeGroupDeleteNodesResponse, error) {
	g, err := p.group(req.GetId())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	var recs []state.Record
	for _, n := range req.GetNodes() {
		rec, ok := p.recordFor(n.GetProviderID())
		if !ok {
			return nil, status.Errorf(codes.NotFound, "node %s (%s) was not created by this provider", n.GetName(), n.GetProviderID())
		}
		if rec.Group != g.Name {
			return nil, status.Errorf(codes.InvalidArgument, "node %s belongs to %s, not %s", n.GetName(), rec.Group, g.Name)
		}
		recs = append(recs, rec)
	}
	// only instances counted in the target size lower it; Failed ones are already outside it
	counted := 0
	for _, r := range recs {
		if r.Phase == state.Creating || r.Phase == state.Running {
			counted++
		}
	}
	if p.targetSize(g.Name)-counted < g.MinSize {
		return nil, status.Errorf(codes.FailedPrecondition, "deleting %d nodes would take %s below minSize %d", counted, g.Name, g.MinSize)
	}
	for _, r := range recs {
		if err := p.startDelete(ctx, r); err != nil {
			return nil, status.Errorf(codes.Unavailable, "delete %s: %v", r.Name, err)
		}
	}
	return &protos.NodeGroupDeleteNodesResponse{}, nil
}

// NodeGroupDecreaseTargetSize only drops instances that have not become nodes yet (cluster-autoscaler calls it
// to cancel a scale up that is not needed any more).
func (p *Provider) NodeGroupDecreaseTargetSize(ctx context.Context, req *protos.NodeGroupDecreaseTargetSizeRequest) (*protos.NodeGroupDecreaseTargetSizeResponse, error) {
	g, err := p.group(req.GetId())
	if err != nil {
		return nil, err
	}
	delta := -int(req.GetDelta())
	if delta <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta must be negative, got %d", req.GetDelta())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var pending []state.Record
	for _, r := range p.Store.List(g.Name) {
		if r.Phase == state.Creating {
			pending = append(pending, r)
		}
	}
	if delta > len(pending) {
		return nil, status.Errorf(codes.FailedPrecondition, "only %d instances of %s are still being created, cannot cancel %d", len(pending), g.Name, delta)
	}
	// newest first: the oldest are the most likely to be about to register
	for i := 0; i < delta; i++ {
		if err := p.startDelete(ctx, pending[len(pending)-1-i]); err != nil {
			return nil, status.Errorf(codes.Unavailable, "cancel %s: %v", pending[len(pending)-1-i].Name, err)
		}
	}
	return &protos.NodeGroupDecreaseTargetSizeResponse{}, nil
}

func (p *Provider) startDelete(ctx context.Context, r state.Record) error {
	if r.Phase == state.Deleting {
		return nil
	}
	now := p.Now()
	if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
		x.Phase, x.DeletingAt, x.TokenHash = state.Deleting, &now, ""
		return true
	}); err != nil {
		return err
	}
	p.Log.Info("deleting instance", "group", r.Group, "node", r.Name, "id", r.ID)
	if r.ID == "" {
		return nil // never reached the cloud; reconcile drops the record
	}
	if err := p.Driver.Delete(ctx, r.ID); err != nil && !errors.Is(err, cloud.ErrRetryLater) {
		return err // stays Deleting; reconcile retries
	}
	return nil
}

// ---- instances, template, options --------------------------------------------------------------------------

func (p *Provider) NodeGroupNodes(_ context.Context, req *protos.NodeGroupNodesRequest) (*protos.NodeGroupNodesResponse, error) {
	if _, err := p.group(req.GetId()); err != nil {
		return nil, err
	}
	out := &protos.NodeGroupNodesResponse{}
	for _, r := range p.Store.List(req.GetId()) {
		id := r.ProviderID
		if id == "" {
			id = p.Driver.ProviderIDPrefix() + "pending/" + r.Name
		}
		st := &protos.InstanceStatus{}
		switch r.Phase {
		case state.Creating:
			st.InstanceState = protos.InstanceStatus_instanceCreating
		case state.Running:
			st.InstanceState = protos.InstanceStatus_instanceRunning
		case state.Deleting:
			st.InstanceState = protos.InstanceStatus_instanceDeleting
		case state.Failed:
			// a creating instance with an error makes cluster-autoscaler give up on it, delete it and back off
			st.InstanceState = protos.InstanceStatus_instanceCreating
			st.ErrorInfo = &protos.InstanceErrorInfo{ErrorCode: "provisioning-failed", ErrorMessage: r.Message, InstanceErrorClass: 99}
		}
		out.Instances = append(out.Instances, &protos.Instance{Id: id, Status: st})
	}
	return out, nil
}

// NodeGroupTemplateNodeInfo describes a node of the group before one exists, so scale from zero works.
func (p *Provider) NodeGroupTemplateNodeInfo(_ context.Context, req *protos.NodeGroupTemplateNodeInfoRequest) (*protos.NodeGroupTemplateNodeInfoResponse, error) {
	g, err := p.group(req.GetId())
	if err != nil {
		return nil, err
	}
	name := g.NamePrefix + "-template"
	labels := map[string]string{
		config.GroupLabel:         g.Name,
		"kubernetes.io/hostname":  name,
		"kubernetes.io/os":        "linux",
		"kubernetes.io/arch":      "amd64",
		"beta.kubernetes.io/os":   "linux",
		"beta.kubernetes.io/arch": "amd64",
	}
	for k, v := range g.Labels {
		labels[k] = v
	}
	taints, err := parseTaints(g.Taints)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	res := corev1.ResourceList{
		corev1.ResourceCPU:    g.Resources.CPU,
		corev1.ResourceMemory: g.Resources.Memory,
		corev1.ResourcePods:   g.Resources.Pods,
	}
	if !g.Resources.EphemeralStorage.IsZero() {
		res[corev1.ResourceEphemeralStorage] = g.Resources.EphemeralStorage
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status: corev1.NodeStatus{
			Capacity:    res,
			Allocatable: res.DeepCopy(),
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	return &protos.NodeGroupTemplateNodeInfoResponse{NodeInfo: node}, nil
}

// NodeGroupGetOptions: Unimplemented makes cluster-autoscaler use its global flags for every group.
func (p *Provider) NodeGroupGetOptions(context.Context, *protos.NodeGroupAutoscalingOptionsRequest) (*protos.NodeGroupAutoscalingOptionsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "per node group options are not supported")
}

func (p *Provider) GPULabel(context.Context, *protos.GPULabelRequest) (*protos.GPULabelResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GPU node groups are not supported")
}

func (p *Provider) GetAvailableGPUTypes(context.Context, *protos.GetAvailableGPUTypesRequest) (*protos.GetAvailableGPUTypesResponse, error) {
	return &protos.GetAvailableGPUTypesResponse{}, nil
}

// Refresh is called before every cluster-autoscaler loop; state is kept fresh by the reconcile loop instead,
// so this stays cheap and well inside the gRPC timeout.
func (p *Provider) Refresh(context.Context, *protos.RefreshRequest) (*protos.RefreshResponse, error) {
	return &protos.RefreshResponse{}, nil
}

func (p *Provider) Cleanup(context.Context, *protos.CleanupRequest) (*protos.CleanupResponse, error) {
	return &protos.CleanupResponse{}, nil
}

// ---- helpers -----------------------------------------------------------------------------------------------

// recordFor resolves an instance ID as reported by NodeGroupNodes, including the placeholder of an instance
// whose VM has no cloud ID yet (cluster-autoscaler deletes those when they time out).
func (p *Provider) recordFor(providerID string) (state.Record, bool) {
	if name, ok := cutPrefix(providerID, p.Driver.ProviderIDPrefix()+"pending/"); ok {
		return p.Store.Get(name)
	}
	return p.Store.ByProviderID(providerID)
}

func (p *Provider) group(id string) (config.NodeGroup, error) {
	g, ok := p.Config.Group(id)
	if !ok {
		return g, status.Errorf(codes.NotFound, "no node group %q", id)
	}
	return g, nil
}

func pbGroup(g config.NodeGroup) *protos.NodeGroup {
	return &protos.NodeGroup{Id: g.Name, MinSize: int32(g.MinSize), MaxSize: int32(g.MaxSize),
		Debug: fmt.Sprintf("%s [%d..%d]", g.Name, g.MinSize, g.MaxSize)}
}

func parseTaints(in []string) ([]corev1.Taint, error) {
	out := make([]corev1.Taint, 0, len(in))
	for _, t := range in {
		kv, effect, ok := strings.Cut(t, ":")
		if !ok {
			return nil, fmt.Errorf("bad taint %q", t)
		}
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, corev1.Taint{Key: k, Value: v, Effect: corev1.TaintEffect(effect)})
	}
	return out, nil
}

// RandomSuffix returns 6 lowercase alphanumerics for VM/node names.
func RandomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
