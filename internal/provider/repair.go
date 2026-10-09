package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

// Node repair: a Running instance whose node stopped working is replaced by the provider itself, keeping the
// target size, so it works at minSize and at maxSize and cluster-autoscaler does not back the group off.
//
//   - VM still running (kubelet, docker or network broken): create first. The old instance becomes Replacing
//     (outside the target size) and is deleted once the replacement is Ready. If the old node recovers before
//     that, it is kept and the replacement is cancelled, so the target size never exceeds what it was.
//   - VM gone, stopped or in error, or Node object gone: the old instance has nothing left to serve, so it is
//     deleted while the replacement is created.
//   - Group above minSize: the broken instance is only deleted, see repair.
//
// At most one repair runs per group at a time, and none while too many nodes are unhealthy (circuit breaker).
// See docs/node-repair.md.

// repairNotReady handles a Running instance whose Node exists but is not Ready.
func (p *Provider) repairNotReady(ctx context.Context, r state.Record, n corev1.Node, v *view) error {
	g, ok := p.Config.Group(r.Group)
	if !ok {
		return nil
	}
	rp := p.Config.RepairFor(g)
	now := p.Now()
	if !rp.On() {
		return nil
	}
	since, ok := notReadySince(n)
	if !ok {
		return nil
	}
	// the episode starts at the first NotReady seen; lastTransitionTime alone restarts on False <-> Unknown
	if r.NotReadySince == nil || since.Before(*r.NotReadySince) {
		if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool { x.NotReadySince = &since; return true }); err != nil {
			return err
		}
	} else {
		since = *r.NotReadySince
	}
	if r.RepairAfter != nil && now.Before(*r.RepairAfter) {
		return nil
	}
	notReady := now.Sub(since)
	// cheap checks first: the cloud API is only asked once a repair could actually start
	if notReady < min(rp.VMGoneAfter.Duration, rp.NotReadyAfter.Duration) || p.repairBlocked(ctx, g, r, v) {
		return nil
	}
	vm, err := p.Driver.Get(ctx, r.ID)
	dead, why := false, ""
	switch {
	case errors.Is(err, cloud.ErrNotFound):
		dead, why = true, "VM is gone"
	case err != nil:
		return err
	case vm.Phase == cloud.PhaseStopped || vm.Phase == cloud.PhaseError || vm.Phase == cloud.PhaseDeleting:
		dead, why = true, "VM is "+vm.RawStatus
	}
	wait := rp.NotReadyAfter.Duration
	if dead {
		wait = rp.VMGoneAfter.Duration
	}
	if notReady < wait {
		return nil
	}
	if !dead {
		why = "node NotReady for " + notReady.Round(time.Second).String() + " while its VM runs"
	}
	return p.repair(ctx, g, r, dead, why)
}

// repair removes a broken instance. Above minSize it is only deleted: the group shrinks by one, and
// cluster-autoscaler scales up again if pods need the room (they become Pending, no backoff). Replacing it there
// would create a VM the autoscaler may be about to scale down anyway. At minSize (where cluster-autoscaler
// neither removes nor replaces a broken node) it is replaced, keeping the size.
func (p *Provider) repair(ctx context.Context, g config.NodeGroup, r state.Record, dead bool, why string) error {
	if size := p.targetSize(g.Name); size > g.MinSize {
		p.Log.Warn("removing broken instance without replacement (group above minSize)", "node", r.Name, "group", g.Name,
			"reason", why, "size", size, "minSize", g.MinSize)
		if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool { x.Message = why; return true }); err != nil {
			return err
		}
		cur, _ := p.Store.Get(r.Name)
		return p.startDelete(ctx, cur)
	}
	return p.startReplace(ctx, g, r, dead, why)
}

// repairBlocked reports (and logs) whether a repair of r must wait: another repair of the group is running, or
// the circuit breaker is open.
func (p *Provider) repairBlocked(ctx context.Context, g config.NodeGroup, r state.Record, v *view) bool {
	if busy := p.repairInProgress(g.Name); busy != "" {
		p.Log.Info("repair waits: another repair of the group is in progress", "node", r.Name, "group", g.Name, "repairing", busy)
		return true
	}
	msg, err := p.overRepairBudget(ctx, g, v)
	if err != nil {
		p.Log.Error("repair waits: cannot evaluate the circuit breaker", "node", r.Name, "err", err)
		return true
	}
	if msg != "" {
		p.Log.Warn("repair blocked by the circuit breaker", "node", r.Name, "group", g.Name, "why", msg)
		return true
	}
	return false
}

// startReplace starts the replacement of r. dead: the old instance is deleted at once instead of after the
// replacement is Ready.
func (p *Provider) startReplace(ctx context.Context, g config.NodeGroup, r state.Record, dead bool, why string) error {
	name := p.pickNames(g, 1)[0]
	p.Log.Warn("replacing instance", "node", r.Name, "group", g.Name, "replacement", name, "reason", why, "deleteFirst", dead)

	// recorded before the create: after a crash in between, the Replacing record points at no replacement and
	// reconcile turns it back into Running
	if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
		x.Phase, x.ReplacedBy, x.Message = state.Replacing, name, why
		return true
	}); err != nil {
		return err
	}
	if err := p.createOne(ctx, g, name, r.Name); err != nil {
		retry := p.Now().Add(createRetryAfter)
		if uerr := p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
			x.Phase, x.ReplacedBy, x.RepairAfter = state.Running, "", &retry
			x.Message = "replacement could not be created: " + err.Error()
			return true
		}); uerr != nil {
			// left Replacing with a dangling link: reconcileReplacing resets it, also with a retry delay
			return fmt.Errorf("create replacement for %s: %w (resetting the record failed: %v)", r.Name, err, uerr)
		}
		return fmt.Errorf("create replacement for %s: %w", r.Name, err)
	}
	if dead {
		cur, _ := p.Store.Get(r.Name)
		return p.startDelete(ctx, cur)
	}
	return nil
}

// reconcileReplacing waits for the replacement of r to become Ready, then deletes r.
func (p *Provider) reconcileReplacing(ctx context.Context, r state.Record, v *view) error {
	now := p.Now()
	nr, ok := p.Store.Get(r.ReplacedBy)
	switch {
	case !ok || nr.Phase == state.Failed || nr.Phase == state.Deleting:
		// the replacement did not make it: keep the old instance and try again later
		retry, msg := now.Add(createRetryAfter), "replacement "+r.ReplacedBy+" was never created"
		if ok {
			g, _ := p.Config.Group(r.Group)
			retry, msg = now.Add(p.Config.RepairFor(g).RetryAfter.Duration), "replacement "+nr.Name+" failed: "+nr.Message
		}
		p.Log.Warn("replacement failed, keeping the instance", "node", r.Name, "replacement", r.ReplacedBy, "retryAfter", retry)
		return p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
			x.Phase, x.ReplacedBy, x.RepairAfter, x.Message = state.Running, "", &retry, msg
			return true
		})
	case nr.Phase == state.Running:
		p.Log.Info("replacement is Ready, deleting the old instance", "node", r.Name, "replacement", nr.Name)
		return p.startDelete(ctx, r)
	}
	// replacement still being created. If the old node recovered meanwhile, keep it and cancel the replacement:
	// keeping both would put the target size one above what it was (above maxSize for a full group).
	if n, ok := p.nodeOf(ctx, r, v); ok && nodeReady(n) {
		p.Log.Info("node recovered during its repair; keeping it and cancelling the replacement", "node", r.Name, "replacement", nr.Name)
		if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
			x.Phase, x.ReplacedBy, x.Message, x.NotReadySince = state.Running, "", "", nil
			return true
		}); err != nil {
			return err
		}
		return p.startDelete(ctx, nr)
	}
	return nil
}

// repairInProgress returns the name of an instance of the group being repaired, or "".
func (p *Provider) repairInProgress(group string) string {
	for _, r := range p.Store.List(group) {
		if r.Phase == state.Replacing || (r.Phase == state.Creating && r.Replaces != "") {
			return r.Name
		}
	}
	return ""
}

// overRepairBudget explains why repairs are blocked, or returns "". Many nodes failing together points at
// the network or the control plane, which new VMs do not fix; one unhealthy node is always repairable.
// The group check uses the group's maxUnhealthyPercent, the cluster check always the top-level one, so a group
// override cannot switch off the cluster-wide protection.
func (p *Provider) overRepairBudget(ctx context.Context, g config.NodeGroup, v *view) (string, error) {
	allowed := func(total, pct int) int { return max(1, total*pct/100) }

	pct := p.Config.RepairFor(g).MaxUnhealthyPercent
	total, unhealthy := 0, 0
	for _, r := range p.Store.List(g.Name) {
		switch r.Phase {
		case state.Replacing:
			total++
			unhealthy++
		case state.Running:
			total++
			if n, ok := p.nodeOf(ctx, r, v); !ok || !nodeReady(n) {
				unhealthy++
			}
		}
	}
	if unhealthy > allowed(total, pct) {
		return fmt.Sprintf("%d of %d nodes of the group are unhealthy (max %d%%)", unhealthy, total, pct), nil
	}

	// the whole cluster is only listed here, when a repair is about to start
	all, err := p.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	unready := 0
	for _, n := range all.Items {
		if !nodeReady(n) {
			unready++
		}
	}
	cpct := p.Config.Repair.MaxUnhealthyPercent
	if unready > allowed(len(all.Items), cpct) {
		return fmt.Sprintf("%d of %d nodes of the cluster are not Ready (max %d%%)", unready, len(all.Items), cpct), nil
	}
	return "", nil
}
