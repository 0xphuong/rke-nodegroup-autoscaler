// Package cloud defines what the provider needs from an IaaS: create, inspect and delete one VM.
//
// Listing VMs is deliberately not part of the interface (the VNG Cloud SDK has no list call); the provider
// keeps its own record of every VM it created, see package state.
package cloud

import (
	"context"
	"errors"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
)

// ErrNotFound is returned by Get when the VM does not exist (any more).
var ErrNotFound = errors.New("instance not found")

// ErrRetryLater is returned by Delete when the cloud refuses for now (e.g. the VM is still being created);
// the reconcile loop tries again.
var ErrRetryLater = errors.New("cloud asked to retry later")

type Phase string

const (
	PhaseCreating Phase = "Creating"
	PhaseRunning  Phase = "Running"
	PhaseDeleting Phase = "Deleting"
	// PhaseStopped: the VM exists but is powered off (stopped by hand, or by the cloud).
	PhaseStopped Phase = "Stopped"
	PhaseError   Phase = "Error"
)

type Instance struct {
	ID         string
	Name       string
	Phase      Phase
	RawStatus  string
	InternalIP string
}

type CreateRequest struct {
	Name     string
	UserData string
	Tags     map[string]string
}

type Driver interface {
	// ProviderIDPrefix is prepended to the instance ID to form the node's spec.providerID, e.g. "vngcloud://".
	ProviderIDPrefix() string
	Create(ctx context.Context, group config.NodeGroup, req CreateRequest) (Instance, error)
	Get(ctx context.Context, id string) (Instance, error)
	// Delete must be idempotent: a VM that is already gone is not an error.
	Delete(ctx context.Context, id string) error
}
