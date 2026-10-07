package provider

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
)

// orphanRecordAge: a record that never got a cloud ID is dropped after this (the create call crashed midway).
const orphanRecordAge = 5 * time.Minute

// deleteRetryAfter: re-issue the cloud delete if the VM is still there this long after the first attempt.
const deleteRetryAfter = 3 * time.Minute

// Run reconciles every interval until ctx is done.
func (p *Provider) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.ReconcileOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReconcileOnce moves every record towards reality: Creating -> Running/Failed, Deleting -> gone, and removes
// Node objects whose VM disappeared.
func (p *Provider) ReconcileOnce(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	nodes, err := p.groupNodes(ctx)
	if err != nil {
		p.Log.Error("list nodes", "err", err)
		return
	}
	for _, r := range p.Store.List("") {
		if err := p.reconcileRecord(ctx, r, nodes); err != nil {
			p.Log.Error("reconcile", "node", r.Name, "phase", r.Phase, "err", err)
		}
	}
	p.reconcileOrphanNodes(ctx, nodes)
}

// groupNodes returns the Node objects carrying the node group label, by providerID.
func (p *Provider) groupNodes(ctx context.Context) (map[string]corev1.Node, error) {
	list, err := p.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: config.GroupLabel})
	if err != nil {
		return nil, err
	}
	out := map[string]corev1.Node{}
	for _, n := range list.Items {
		if n.Spec.ProviderID != "" {
			out[n.Spec.ProviderID] = n
		}
	}
	return out, nil
}

func (p *Provider) reconcileRecord(ctx context.Context, r state.Record, nodes map[string]corev1.Node) error {
	now := p.Now()
	switch r.Phase {
	case state.Creating:
		if r.ID == "" {
			if now.Sub(r.CreatedAt) > orphanRecordAge {
				p.Log.Warn("dropping record that never got a VM", "node", r.Name)
				return p.Store.Remove(ctx, r.Name)
			}
			return nil
		}
		inst, err := p.Driver.Get(ctx, r.ID)
		if errors.Is(err, cloud.ErrNotFound) {
			return p.fail(ctx, r, "VM disappeared while provisioning")
		}
		if err != nil {
			return err
		}
		if inst.Phase == cloud.PhaseError {
			return p.fail(ctx, r, "VM is in status "+inst.RawStatus)
		}
		if n, ok := nodes[r.ProviderID]; ok && nodeReady(n) {
			p.Log.Info("node registered", "node", r.Name, "group", r.Group)
			return p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
				x.Phase, x.TokenHash, x.Message = state.Running, "", ""
				return true
			})
		}
		if now.Sub(r.CreatedAt) > p.Config.MaxProvisionTime.Duration {
			return p.fail(ctx, r, "no Ready node within maxProvisionTime (see /var/log/rke-nodegroup-bootstrap.log on the VM)")
		}
		return nil

	case state.Running:
		if _, ok := nodes[r.ProviderID]; ok {
			return nil
		}
		// Node object gone: was the VM deleted behind our back?
		if _, err := p.Driver.Get(ctx, r.ID); errors.Is(err, cloud.ErrNotFound) {
			p.Log.Warn("VM and node gone outside the autoscaler, dropping record", "node", r.Name)
			return p.Store.Remove(ctx, r.Name)
		}
		return nil

	case state.Deleting:
		if r.ID != "" {
			_, err := p.Driver.Get(ctx, r.ID)
			switch {
			case err == nil:
				if r.DeletingAt != nil && now.Sub(*r.DeletingAt) > deleteRetryAfter {
					if derr := p.Driver.Delete(ctx, r.ID); derr != nil && !errors.Is(derr, cloud.ErrRetryLater) {
						return derr
					}
					return p.Store.Update(ctx, r.Name, func(x *state.Record) bool { x.DeletingAt = &now; return true })
				}
				return nil // still going away
			case !errors.Is(err, cloud.ErrNotFound):
				return err
			}
		}
		// VM is gone: now the Node object can go without kubelet bringing it back
		if n, ok := nodes[r.ProviderID]; ok {
			if err := p.deleteNode(ctx, n); err != nil {
				return err
			}
		}
		p.Log.Info("instance removed", "node", r.Name, "group", r.Group)
		return p.Store.Remove(ctx, r.Name)

	case state.Failed:
		// reported to cluster-autoscaler, which calls DeleteNodes for it
		return nil
	}
	return nil
}

func (p *Provider) fail(ctx context.Context, r state.Record, msg string) error {
	p.Log.Warn("instance failed", "node", r.Name, "group", r.Group, "reason", msg)
	return p.Store.Update(ctx, r.Name, func(x *state.Record) bool {
		x.Phase, x.Message, x.TokenHash = state.Failed, msg, ""
		return true
	})
}

// reconcileOrphanNodes deletes Node objects with the group label whose VM no longer exists and which the store
// does not know (e.g. the state ConfigMap was lost). A node whose VM still exists is only reported: the
// provider does not adopt or delete VMs it has no record of.
func (p *Provider) reconcileOrphanNodes(ctx context.Context, nodes map[string]corev1.Node) {
	prefix := p.Driver.ProviderIDPrefix()
	for pid, n := range nodes {
		if _, ok := p.Store.ByProviderID(pid); ok {
			continue
		}
		id, ok := cutPrefix(pid, prefix)
		if !ok {
			continue
		}
		_, err := p.Driver.Get(ctx, id)
		switch {
		case errors.Is(err, cloud.ErrNotFound):
			p.Log.Warn("deleting orphan node whose VM is gone", "node", n.Name)
			if err := p.deleteNode(ctx, n); err != nil {
				p.Log.Error("delete orphan node", "node", n.Name, "err", err)
			}
		case err == nil:
			p.Log.Warn("node has the group label but no record; leaving it alone", "node", n.Name, "providerID", pid)
		}
	}
}

func (p *Provider) deleteNode(ctx context.Context, n corev1.Node) error {
	// precondition on UID: never delete a same-named node that was re-created in between
	err := p.Kube.CoreV1().Nodes().Delete(ctx, n.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &n.UID}})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}
