package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

// Rollout: nodes created with an older machine spec of their group (flavor, image, disk, network; see
// config.NodeGroup.SpecHash) are replaced one at a time per group, through the repair machinery:
//
//  1. the oldest outdated Running instance becomes Replacing (Rollout set) and a replacement is created with the
//     current spec. The old node keeps serving meanwhile.
//  2. once the replacement is Ready, the old node is cordoned and its pods evicted (PodDisruptionBudgets hold;
//     DaemonSet and mirror pods stay). Pods without a controller block the drain: they would be lost.
//  3. once drained, the old instance is deleted like any other.
//
// A rollout step only starts while every instance of the group is a Ready Running node: nothing is being
// created (scale up), repaired or failed. A replacement that does not become Ready stops the rollout: the old
// node stays and the next attempt waits repair.retryAfter.

// drainWarnEvery: how often a drain that is blocked (PodDisruptionBudget, pod without controller) is logged.
const drainWarnEvery = 10 * time.Minute

// AdoptSpecs records the current spec of their group on instances created before rollouts existed, so that
// upgrading the provider does not replace every node. A spec changed in the same deploy is not rolled out to them.
func (p *Provider) AdoptSpecs(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.Store.List("") {
		g, ok := p.Config.Group(r.Group)
		if r.Spec != "" || !ok {
			continue
		}
		spec := g.SpecHash()
		if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool { x.Spec = spec; return true }); err != nil {
			return err
		}
		p.Log.Info("instance adopted the current machine spec", "node", r.Name, "group", r.Group, "spec", spec)
	}
	return nil
}

// rollout starts the replacement of one outdated instance of g, if the group is settled.
func (p *Provider) rollout(ctx context.Context, g config.NodeGroup, v *view) error {
	if !p.Config.RolloutFor(g).On() {
		return nil
	}
	want := g.SpecHash()
	now := p.Now()
	var outdated []state.Record
	for _, r := range p.Store.List(g.Name) {
		switch r.Phase {
		case state.Creating, state.Replacing, state.Failed:
			return nil // scale up, repair, rollout step or failure in progress
		case state.Running:
			if n, ok := p.nodeOf(ctx, r, v); !ok || !nodeReady(n) {
				return nil // repair first
			}
			if r.Spec != want && (r.RepairAfter == nil || !now.Before(*r.RepairAfter)) {
				outdated = append(outdated, r)
			}
		}
	}
	if len(outdated) == 0 {
		return nil
	}
	r := outdated[0] // oldest first
	why := fmt.Sprintf("machine spec changed (%s -> %s), %d outdated node(s) left", r.Spec, want, len(outdated))
	return p.startReplace(ctx, g, r, false, true, why)
}

// drainAndDelete drains the old node of a rollout step whose replacement nr is Ready, then deletes it.
func (p *Provider) drainAndDelete(ctx context.Context, r, nr state.Record, v *view) error {
	n, ok := p.nodeOf(ctx, r, v)
	if !ok || !nodeReady(n) {
		// nothing runs there any more: evictions would hang on a dead kubelet
		p.Log.Info("old node gone or NotReady, deleting it without drain", "node", r.Name, "replacement", nr.Name)
		return p.startDelete(ctx, r)
	}
	now := p.Now()
	if r.DrainStartedAt == nil {
		p.Log.Info("replacement is Ready, draining the old node", "node", r.Name, "replacement", nr.Name)
		if err := p.setUnschedulable(ctx, n.Name, true); err != nil {
			return err
		}
		if err := p.Store.Update(ctx, r.Name, func(x *state.Record) bool { x.DrainStartedAt = &now; return true }); err != nil {
			return err
		}
		r.DrainStartedAt = &now
	} else if !n.Spec.Unschedulable {
		if err := p.setUnschedulable(ctx, n.Name, true); err != nil { // uncordoned by hand meanwhile
			return err
		}
	}
	left, blocked, err := p.evictPods(ctx, n.Name)
	if err != nil {
		return err
	}
	if left == 0 {
		p.Log.Info("node drained, deleting the old instance", "node", r.Name, "replacement", nr.Name,
			"drainTook", now.Sub(*r.DrainStartedAt).Round(time.Second).String())
		delete(p.drainWarned, r.Name)
		return p.startDelete(ctx, r)
	}
	if len(blocked) > 0 && now.Sub(p.drainWarned[r.Name]) >= drainWarnEvery {
		if p.drainWarned == nil {
			p.drainWarned = map[string]time.Time{}
		}
		p.drainWarned[r.Name] = now
		p.Log.Warn("drain blocked, the old node stays until these pods can go", "node", r.Name,
			"since", r.DrainStartedAt.Format(time.RFC3339), "pods", strings.Join(blocked, "; "))
	}
	return nil
}

// evictPods evicts the pods of a node that a drain moves. It returns how many such pods are still there and,
// for those that cannot be evicted now, why.
func (p *Provider) evictPods(ctx context.Context, node string) (left int, blocked []string, err error) {
	pods, err := p.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return 0, nil, err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != node || !drained(pod) {
			continue
		}
		left++
		id := pod.Namespace + "/" + pod.Name
		if pod.DeletionTimestamp != nil {
			continue // already going
		}
		if metav1.GetControllerOf(&pod) == nil {
			blocked = append(blocked, id+": no controller, it would not come back (delete it by hand)")
			continue
		}
		err := p.Kube.PolicyV1().Evictions(pod.Namespace).Evict(ctx, &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		})
		switch {
		case err == nil:
			p.Log.Info("evicted pod", "node", node, "pod", id)
		case apierrors.IsNotFound(err):
			left--
		case apierrors.IsTooManyRequests(err):
			blocked = append(blocked, id+": PodDisruptionBudget")
		default:
			blocked = append(blocked, id+": "+err.Error())
		}
	}
	return left, blocked, nil
}

// drained reports whether a drain has to move the pod: finished pods, DaemonSet pods and static (mirror) pods
// stay with the node.
func drained(pod corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]; ok {
		return false
	}
	if c := metav1.GetControllerOf(&pod); c != nil && c.Kind == "DaemonSet" {
		return false
	}
	return true
}

func (p *Provider) setUnschedulable(ctx context.Context, node string, on bool) error {
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, on)
	_, err := p.Kube.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
