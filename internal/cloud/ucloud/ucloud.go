// Package ucloud implements cloud.Driver with the UCloud UHost API (github.com/ucloud/ucloud-sdk-go).
//
// UCloud only terminates a stopped host, so Delete powers a running host off and asks to be called again
// (cloud.ErrRetryLater); the reconcile loop calls it again once the host reports Stopped.
package ucloud

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"

	"github.com/ucloud/ucloud-sdk-go/services/uhost"
	"github.com/ucloud/ucloud-sdk-go/ucloud"
	"github.com/ucloud/ucloud-sdk-go/ucloud/auth"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
)

const providerIDPrefix = "ucloud://"

// maxUserData is the CreateUHostInstance limit on user data (before base64).
const maxUserData = 16 * 1024

// UHost states (DescribeUHostInstance State).
const (
	stateInitializing = "Initializing"
	stateStarting     = "Starting"
	stateRunning      = "Running"
	stateStopping     = "Stopping"
	stateStopped      = "Stopped"
	stateInstallFail  = "Install Fail"
	stateRebooting    = "Rebooting"
)

// api is the part of the UHost client the driver uses (a fake in tests).
type api interface {
	NewCreateUHostInstanceRequest() *uhost.CreateUHostInstanceRequest
	CreateUHostInstance(*uhost.CreateUHostInstanceRequest) (*uhost.CreateUHostInstanceResponse, error)
	NewDescribeUHostInstanceRequest() *uhost.DescribeUHostInstanceRequest
	DescribeUHostInstance(*uhost.DescribeUHostInstanceRequest) (*uhost.DescribeUHostInstanceResponse, error)
	NewPoweroffUHostInstanceRequest() *uhost.PoweroffUHostInstanceRequest
	PoweroffUHostInstance(*uhost.PoweroffUHostInstanceRequest) (*uhost.PoweroffUHostInstanceResponse, error)
	NewTerminateUHostInstanceRequest() *uhost.TerminateUHostInstanceRequest
	TerminateUHostInstance(*uhost.TerminateUHostInstanceRequest) (*uhost.TerminateUHostInstanceResponse, error)
}

type Driver struct {
	api       api
	region    string
	projectID string
}

// New builds a driver from config plus UCLOUD_PUBLIC_KEY / UCLOUD_PRIVATE_KEY (an API key of a sub-account
// limited to UHost create, describe, power off and terminate).
func New(c *config.UCloud) (*Driver, error) {
	pub, priv := os.Getenv("UCLOUD_PUBLIC_KEY"), os.Getenv("UCLOUD_PRIVATE_KEY")
	if pub == "" || priv == "" {
		return nil, errors.New("UCLOUD_PUBLIC_KEY and UCLOUD_PRIVATE_KEY must be set")
	}
	cfg := ucloud.NewConfig()
	cfg.Region = c.Region
	cfg.ProjectId = c.ProjectID
	if c.APIEndpoint != "" {
		cfg.BaseUrl = c.APIEndpoint
	}
	cfg.UserAgent = "rke-nodegroup-autoscaler"
	cred := auth.NewCredential()
	cred.PublicKey, cred.PrivateKey = pub, priv
	return &Driver{api: uhost.NewClient(&cfg, &cred), region: c.Region, projectID: c.ProjectID}, nil
}

func (d *Driver) ProviderIDPrefix() string { return providerIDPrefix }

func (d *Driver) Create(_ context.Context, g config.NodeGroup, req cloud.CreateRequest) (cloud.Instance, error) {
	s := g.UCloud
	if s == nil {
		return cloud.Instance{}, fmt.Errorf("node group %s has no ucloud settings", g.Name)
	}
	if len(req.UserData) > maxUserData {
		return cloud.Instance{}, fmt.Errorf("ucloud create %s: user data is %d bytes, UCloud accepts at most %d; shorten bootstrap.cloudConfig/initScripts", req.Name, len(req.UserData), maxUserData)
	}
	r := d.api.NewCreateUHostInstanceRequest()
	d.scope(&r.Region, &r.ProjectId)
	r.Zone = ucloud.String(s.Zone)
	r.Name = ucloud.String(req.Name)
	r.ImageId = ucloud.String(s.ImageID)
	r.MachineType = ucloud.String(s.MachineType)
	r.CPU = ucloud.Int(s.CPU)
	r.Memory = ucloud.Int(s.MemoryMB)
	if s.MinimalCPUPlatform != "" {
		r.MinimalCpuPlatform = ucloud.String(s.MinimalCPUPlatform)
	}
	r.Disks = []uhost.UHostDisk{{IsBoot: ucloud.String("True"), Type: ucloud.String(s.BootDiskType), Size: ucloud.Int(s.BootDiskSize)}}
	r.VPCId = ucloud.String(s.VPCID)
	r.SubnetId = ucloud.String(s.SubnetID)
	switch {
	case len(s.SecGroupIDs) > 0:
		r.SecurityMode = ucloud.String("SecGroup")
		for i, id := range s.SecGroupIDs {
			r.SecGroupId = append(r.SecGroupId, uhost.CreateUHostInstanceParamSecGroupId{Id: ucloud.String(id), Priority: ucloud.Int(i + 1)})
		}
	case s.SecurityGroupID != "":
		r.SecurityMode = ucloud.String("Firewall")
		r.SecurityGroupId = ucloud.String(s.SecurityGroupID)
	}
	if s.KeyPairID != "" {
		r.LoginMode = ucloud.String("KeyPair")
		r.KeyPairId = ucloud.String(s.KeyPairID)
	} else {
		pw, err := randomPassword()
		if err != nil {
			return cloud.Instance{}, err
		}
		r.LoginMode = ucloud.String("Password")
		r.Password = ucloud.String(base64.StdEncoding.EncodeToString([]byte(pw)))
	}
	r.ChargeType = ucloud.String("Dynamic")
	if s.ChargeType != "" {
		r.ChargeType = ucloud.String(s.ChargeType)
	}
	if s.IsolationGroup != "" {
		r.IsolationGroup = ucloud.String(s.IsolationGroup)
	}
	if s.BusinessGroup != "" {
		r.Tag = ucloud.String(s.BusinessGroup)
	}
	r.Labels = labels(req.Tags, s.Tags)
	r.UserData = ucloud.String(base64.StdEncoding.EncodeToString([]byte(req.UserData)))

	resp, err := d.api.CreateUHostInstance(r)
	if err != nil {
		return cloud.Instance{}, fmt.Errorf("ucloud create %s: %w", req.Name, err)
	}
	if resp == nil || len(resp.UHostIds) == 0 || resp.UHostIds[0] == "" {
		return cloud.Instance{}, fmt.Errorf("ucloud create %s: no host id in response", req.Name)
	}
	in := cloud.Instance{ID: resp.UHostIds[0], Name: req.Name, Phase: cloud.PhaseCreating, RawStatus: stateInitializing}
	if len(resp.IPs) > 0 {
		in.InternalIP = resp.IPs[0]
	}
	return in, nil
}

func (d *Driver) Get(_ context.Context, id string) (cloud.Instance, error) {
	h, err := d.describe(id)
	if err != nil {
		return cloud.Instance{}, err
	}
	return toInstance(h), nil
}

// Delete is idempotent: a host that is gone is not an error. A running host is powered off first (UCloud only
// terminates stopped hosts) and ErrRetryLater returned; once it is Stopped the next call terminates it, with
// its disks and EIP.
func (d *Driver) Delete(_ context.Context, id string) error {
	h, err := d.describe(id)
	if errors.Is(err, cloud.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch h.State {
	case stateStopped, stateInstallFail:
		r := d.api.NewTerminateUHostInstanceRequest()
		d.scope(&r.Region, &r.ProjectId)
		r.Zone = ucloud.String(h.Zone)
		r.UHostId = ucloud.String(id)
		r.ReleaseUDisk = ucloud.Bool(true)
		r.ReleaseEIP = ucloud.Bool(true)
		if _, err := d.api.TerminateUHostInstance(r); err != nil {
			return fmt.Errorf("ucloud terminate %s: %w", id, err)
		}
		return nil
	case stateStopping, stateInitializing:
		// wait: still stopping, or not yet able to be powered off
		return cloud.ErrRetryLater
	default:
		r := d.api.NewPoweroffUHostInstanceRequest()
		d.scope(&r.Region, &r.ProjectId)
		r.Zone = ucloud.String(h.Zone)
		r.UHostId = ucloud.String(id)
		if _, err := d.api.PoweroffUHostInstance(r); err != nil {
			return fmt.Errorf("ucloud power off %s: %w", id, err)
		}
		return cloud.ErrRetryLater
	}
}

func (d *Driver) describe(id string) (uhost.UHostInstanceSet, error) {
	r := d.api.NewDescribeUHostInstanceRequest()
	d.scope(&r.Region, &r.ProjectId)
	r.UHostIds = []string{id}
	resp, err := d.api.DescribeUHostInstance(r)
	if err != nil {
		return uhost.UHostInstanceSet{}, fmt.Errorf("ucloud describe %s: %w", id, err)
	}
	for _, h := range resp.UHostSet {
		if h.UHostId == id {
			return h, nil
		}
	}
	// an unknown or terminated id comes back as an empty set, not an error
	return uhost.UHostInstanceSet{}, cloud.ErrNotFound
}

func (d *Driver) scope(region, project **string) {
	*region = ucloud.String(d.region)
	if d.projectID != "" {
		*project = ucloud.String(d.projectID)
	}
}

// toInstance maps a UHost to the provider's view. Booting and rebooting hosts count as creating (alive), not as
// dead: only Stopping/Stopped and Install Fail make the repair treat the VM as gone.
func toInstance(h uhost.UHostInstanceSet) cloud.Instance {
	in := cloud.Instance{ID: h.UHostId, Name: h.Name, RawStatus: h.State}
	switch h.State {
	case stateRunning:
		in.Phase = cloud.PhaseRunning
	case stateStopping, stateStopped:
		in.Phase = cloud.PhaseStopped
	case stateInstallFail:
		in.Phase = cloud.PhaseError
	default: // Initializing, Starting, Rebooting, "" (state unknown)
		in.Phase = cloud.PhaseCreating
	}
	for _, ip := range h.IPSet {
		if ip.Type == "Private" && ip.IP != "" {
			in.InternalIP = ip.IP
			break
		}
	}
	return in
}

func labels(maps ...map[string]string) []uhost.CreateUHostInstanceParamLabels {
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
	out := make([]uhost.CreateUHostInstanceParamLabels, 0, len(keys))
	for _, k := range keys {
		out = append(out, uhost.CreateUHostInstanceParamLabels{Key: ucloud.String(k), Value: ucloud.String(merged[k])})
	}
	return out
}

// randomPassword returns a 24 character password with upper, lower, digit and symbol, for hosts created
// without a key pair. Nobody keeps it: log in through the SSH keys of the site cloud-init instead.
func randomPassword() (string, error) {
	const (
		lower  = "abcdefghijkmnopqrstuvwxyz"
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		digits = "23456789"
		symbol = "!@#%^*-_+="
		all    = lower + upper + digits + symbol
	)
	pick := func(set string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[n.Int64()], nil
	}
	b := make([]byte, 0, 24)
	for _, set := range []string{lower, upper, digits, symbol} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		b = append(b, c)
	}
	for len(b) < 24 {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		b = append(b, c)
	}
	return string(b), nil
}
