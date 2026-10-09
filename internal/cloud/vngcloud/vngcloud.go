// Package vngcloud implements cloud.Driver with the VNG Cloud vServer API (github.com/vngcloud/vngcloud-go-sdk).
package vngcloud

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	lsclient "github.com/vngcloud/vngcloud-go-sdk/v2/client"
	lsentity "github.com/vngcloud/vngcloud-go-sdk/v2/vngcloud/entity"
	lserr "github.com/vngcloud/vngcloud-go-sdk/v2/vngcloud/sdk_error"
	compute "github.com/vngcloud/vngcloud-go-sdk/v2/vngcloud/services/compute/v2"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
)

const providerIDPrefix = "vngcloud://"

type Driver struct {
	// the SDK client re-authenticates under the hood; serialize calls rather than rely on it being goroutine-safe
	mu  sync.Mutex
	cli lsclient.IClient
}

// New builds a driver from config plus VNGCLOUD_CLIENT_ID / VNGCLOUD_CLIENT_SECRET (a service account of the
// project, scoped to creating and deleting servers).
func New(ctx context.Context, c *config.VNGCloud) (*Driver, error) {
	id, secret := os.Getenv("VNGCLOUD_CLIENT_ID"), os.Getenv("VNGCLOUD_CLIENT_SECRET")
	if id == "" || secret == "" {
		return nil, errors.New("VNGCLOUD_CLIENT_ID and VNGCLOUD_CLIENT_SECRET must be set")
	}
	sdkCfg := lsclient.NewSdkConfigure().
		WithClientId(id).
		WithClientSecret(secret).
		WithProjectId(c.ProjectID).
		WithZoneId(c.ZoneID).
		WithIamEndpoint(c.IAMEndpoint).
		WithVServerEndpoint(c.VServerEndpoint).
		WithUserAgent("rke-nodegroup-autoscaler")
	cli := lsclient.NewClient(ctx).WithRetryCount(2).WithSleep(5).Configure(sdkCfg)
	return &Driver{cli: cli}, nil
}

func (d *Driver) ProviderIDPrefix() string { return providerIDPrefix }

func (d *Driver) Create(_ context.Context, g config.NodeGroup, req cloud.CreateRequest) (cloud.Instance, error) {
	s := g.VNGCloud
	opt := compute.NewCreateServerRequest(req.Name, s.ImageID, s.FlavorID, s.RootDiskTypeID, s.RootDiskSize).
		WithNetwork(s.NetworkID, s.SubnetID).
		WithUserData(req.UserData, false).
		WithTags(flattenTags(req.Tags, s.Tags)...)
	if len(s.SecurityGroups) > 0 {
		opt = opt.WithSecgroups(s.SecurityGroups...)
	}
	if s.ServerGroupID != "" {
		opt = opt.WithServerGroupId(s.ServerGroupID)
	}
	if s.ZoneID != "" {
		opt = opt.WithZone(s.ZoneID)
	}
	if s.SSHKeyID != "" {
		// the request interface has no setter for it; the concrete type is exported
		if r, ok := opt.(*compute.CreateServerRequest); ok {
			r.SshKeyId = s.SSHKeyID
		}
	}

	d.mu.Lock()
	srv, sdkErr := d.cli.VServerGateway().V2().ComputeService().CreateServer(opt)
	d.mu.Unlock()
	if sdkErr != nil {
		return cloud.Instance{}, fmt.Errorf("vngcloud create server %s: %s", req.Name, describe(sdkErr))
	}
	if srv == nil || srv.Uuid == "" {
		return cloud.Instance{}, fmt.Errorf("vngcloud create server %s: no server id in response", req.Name)
	}
	return toInstance(srv), nil
}

func (d *Driver) Get(_ context.Context, id string) (cloud.Instance, error) {
	d.mu.Lock()
	srv, sdkErr := d.cli.VServerGateway().V2().ComputeService().GetServerById(compute.NewGetServerByIdRequest(id))
	d.mu.Unlock()
	if sdkErr != nil {
		if sdkErr.IsError(lserr.EcVServerServerNotFound) {
			return cloud.Instance{}, cloud.ErrNotFound
		}
		return cloud.Instance{}, fmt.Errorf("vngcloud get server %s: %s", id, describe(sdkErr))
	}
	return toInstance(srv), nil
}

func (d *Driver) Delete(_ context.Context, id string) error {
	// root disk goes with the VM; node groups are cattle
	req := compute.NewDeleteServerByIdRequest(id).WithDeleteAllVolume(true)
	d.mu.Lock()
	sdkErr := d.cli.VServerGateway().V2().ComputeService().DeleteServerById(req)
	d.mu.Unlock()
	switch {
	case sdkErr == nil:
		return nil
	case sdkErr.IsErrorAny(lserr.EcVServerServerNotFound, lserr.EcVServerServerDeleteDeletingServer):
		return nil
	case sdkErr.IsError(lserr.EcVServerServerDeleteCreatingServer):
		return cloud.ErrRetryLater
	default:
		return fmt.Errorf("vngcloud delete server %s: %s", id, describe(sdkErr))
	}
}

// toInstance maps a vServer to the provider's view. vServer status strings are not documented in the SDK;
// only ACTIVE counts as running, anything mentioning ERROR as failed, DELET* as deleting, STOP*/SHUTOFF as
// stopped.
func toInstance(s *lsentity.Server) cloud.Instance {
	in := cloud.Instance{ID: s.Uuid, Name: s.Name, RawStatus: s.Status}
	st := strings.ToUpper(s.Status)
	switch {
	case st == "ACTIVE":
		in.Phase = cloud.PhaseRunning
	case strings.Contains(st, "ERROR"):
		in.Phase = cloud.PhaseError
	case strings.HasPrefix(st, "DELET"):
		in.Phase = cloud.PhaseDeleting
	case strings.HasPrefix(st, "STOP"), st == "SHUTOFF":
		in.Phase = cloud.PhaseStopped
	default:
		in.Phase = cloud.PhaseCreating
	}
	for _, nic := range s.InternalInterfaces {
		if nic.FixedIp != "" {
			in.InternalIP = nic.FixedIp
			break
		}
	}
	return in
}

func flattenTags(maps ...map[string]string) []string {
	merged := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			merged[k] = v
		}
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, k, merged[k])
	}
	return out
}

func describe(e lserr.IError) string {
	return fmt.Sprintf("%s: %s", e.GetErrorCode(), strings.TrimSpace(e.GetMessage()))
}
