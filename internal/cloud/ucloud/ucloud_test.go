package ucloud

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/ucloud/ucloud-sdk-go/services/uhost"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
)

// fakeAPI records requests and serves hosts from a map.
type fakeAPI struct {
	hosts      map[string]*uhost.UHostInstanceSet
	created    *uhost.CreateUHostInstanceRequest
	poweroffs  []string
	terminated []*uhost.TerminateUHostInstanceRequest
	createErr  error
}

func (f *fakeAPI) NewCreateUHostInstanceRequest() *uhost.CreateUHostInstanceRequest {
	return &uhost.CreateUHostInstanceRequest{}
}
func (f *fakeAPI) CreateUHostInstance(r *uhost.CreateUHostInstanceRequest) (*uhost.CreateUHostInstanceResponse, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = r
	f.hosts["uhost-1"] = &uhost.UHostInstanceSet{UHostId: "uhost-1", State: stateInitializing, Zone: *r.Zone}
	return &uhost.CreateUHostInstanceResponse{UHostIds: []string{"uhost-1"}, IPs: []string{"10.0.1.5"}}, nil
}
func (f *fakeAPI) NewDescribeUHostInstanceRequest() *uhost.DescribeUHostInstanceRequest {
	return &uhost.DescribeUHostInstanceRequest{}
}
func (f *fakeAPI) DescribeUHostInstance(r *uhost.DescribeUHostInstanceRequest) (*uhost.DescribeUHostInstanceResponse, error) {
	resp := &uhost.DescribeUHostInstanceResponse{}
	for _, id := range r.UHostIds {
		if h, ok := f.hosts[id]; ok {
			resp.UHostSet = append(resp.UHostSet, *h)
		}
	}
	return resp, nil
}
func (f *fakeAPI) NewPoweroffUHostInstanceRequest() *uhost.PoweroffUHostInstanceRequest {
	return &uhost.PoweroffUHostInstanceRequest{}
}
func (f *fakeAPI) PoweroffUHostInstance(r *uhost.PoweroffUHostInstanceRequest) (*uhost.PoweroffUHostInstanceResponse, error) {
	f.poweroffs = append(f.poweroffs, *r.UHostId)
	f.hosts[*r.UHostId].State = stateStopping
	return &uhost.PoweroffUHostInstanceResponse{}, nil
}
func (f *fakeAPI) NewTerminateUHostInstanceRequest() *uhost.TerminateUHostInstanceRequest {
	return &uhost.TerminateUHostInstanceRequest{}
}
func (f *fakeAPI) TerminateUHostInstance(r *uhost.TerminateUHostInstanceRequest) (*uhost.TerminateUHostInstanceResponse, error) {
	if h := f.hosts[*r.UHostId]; h.State != stateStopped && h.State != stateInstallFail {
		return nil, errors.New("host must be stopped") // what UCloud answers
	}
	f.terminated = append(f.terminated, r)
	delete(f.hosts, *r.UHostId)
	return &uhost.TerminateUHostInstanceResponse{}, nil
}

func newDriver() (*Driver, *fakeAPI) {
	f := &fakeAPI{hosts: map[string]*uhost.UHostInstanceSet{}}
	return &Driver{api: f, region: "sg", projectID: "org-test"}, f
}

func group(s config.UCloudServer) config.NodeGroup {
	return config.NodeGroup{Name: "app", UCloud: &s}
}

var server = config.UCloudServer{
	Zone: "sg-02", ImageID: "uimage-x", MachineType: "O", CPU: 4, MemoryMB: 16384,
	BootDiskType: "CLOUD_RSSD", BootDiskSize: 40, VPCID: "uvnet-x", SubnetID: "subnet-x",
	SecGroupIDs: []string{"secgroup-a", "secgroup-b"}, KeyPairID: "uhostkp-x",
	Tags: map[string]string{"team": "ops"},
}

func TestCreateRequest(t *testing.T) {
	d, f := newDriver()
	in, err := d.Create(context.Background(), group(server), cloud.CreateRequest{
		Name: "dev-app-abc123", UserData: "#!/bin/sh\necho hi\n",
		Tags: map[string]string{"rke-autoscaler-cluster": "dev", "rke-autoscaler-nodegroup": "app"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != "uhost-1" || in.InternalIP != "10.0.1.5" || in.Phase != cloud.PhaseCreating {
		t.Fatalf("instance %+v", in)
	}
	r := f.created
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	for name, got := range map[string][2]string{
		"region":     {str(r.Region), "sg"},
		"project":    {str(r.ProjectId), "org-test"},
		"zone":       {str(r.Zone), "sg-02"},
		"name":       {str(r.Name), "dev-app-abc123"},
		"image":      {str(r.ImageId), "uimage-x"},
		"machine":    {str(r.MachineType), "O"},
		"vpc":        {str(r.VPCId), "uvnet-x"},
		"subnet":     {str(r.SubnetId), "subnet-x"},
		"login":      {str(r.LoginMode), "KeyPair"},
		"keypair":    {str(r.KeyPairId), "uhostkp-x"},
		"charge":     {str(r.ChargeType), "Dynamic"},
		"secmode":    {str(r.SecurityMode), "SecGroup"},
		"bootIsBoot": {str(r.Disks[0].IsBoot), "True"},
		"bootType":   {str(r.Disks[0].Type), "CLOUD_RSSD"},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", name, got[0], got[1])
		}
	}
	if *r.CPU != 4 || *r.Memory != 16384 || *r.Disks[0].Size != 40 || len(r.Disks) != 1 {
		t.Errorf("cpu/memory/disk: %d %d %+v", *r.CPU, *r.Memory, r.Disks)
	}
	if len(r.SecGroupId) != 2 || *r.SecGroupId[1].Id != "secgroup-b" || *r.SecGroupId[1].Priority != 2 {
		t.Errorf("security groups in priority order: %+v", r.SecGroupId)
	}
	if r.Password != nil || r.SecurityGroupId != nil {
		t.Error("no password with a key pair, no firewall with security groups")
	}
	ud, _ := base64.StdEncoding.DecodeString(*r.UserData)
	if string(ud) != "#!/bin/sh\necho hi\n" {
		t.Errorf("user data must be base64 of the script, got %q", ud)
	}
	var keys []string
	for _, l := range r.Labels {
		keys = append(keys, *l.Key+"="+*l.Value)
	}
	if strings.Join(keys, ",") != "rke-autoscaler-cluster=dev,rke-autoscaler-nodegroup=app,team=ops" {
		t.Errorf("labels %v", keys)
	}
}

func TestCreateWithoutKeyPairUsesRandomPasswordAndFirewall(t *testing.T) {
	d, f := newDriver()
	s := server
	s.KeyPairID, s.SecGroupIDs, s.SecurityGroupID, s.ChargeType = "", nil, "firewall-x", "Postpay"
	if _, err := d.Create(context.Background(), group(s), cloud.CreateRequest{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	r := f.created
	pw, _ := base64.StdEncoding.DecodeString(*r.Password)
	if *r.LoginMode != "Password" || len(pw) != 24 || strings.ContainsAny(string(pw), "lIO01") {
		t.Errorf("login %s, password %q", *r.LoginMode, pw)
	}
	if *r.SecurityMode != "Firewall" || *r.SecurityGroupId != "firewall-x" || *r.ChargeType != "Postpay" {
		t.Errorf("security %s %s, charge %s", *r.SecurityMode, *r.SecurityGroupId, *r.ChargeType)
	}
}

func TestCreateRejectsOversizedUserData(t *testing.T) {
	d, f := newDriver()
	_, err := d.Create(context.Background(), group(server), cloud.CreateRequest{Name: "n", UserData: strings.Repeat("x", maxUserData+1)})
	if err == nil || !strings.Contains(err.Error(), "at most") || f.created != nil {
		t.Fatalf("want a clear error before calling the API, got %v", err)
	}
}

func TestGetMapsStates(t *testing.T) {
	d, f := newDriver()
	for state, want := range map[string]cloud.Phase{
		stateInitializing: cloud.PhaseCreating, stateStarting: cloud.PhaseCreating, stateRebooting: cloud.PhaseCreating,
		"":           cloud.PhaseCreating,
		stateRunning: cloud.PhaseRunning, stateStopping: cloud.PhaseStopped, stateStopped: cloud.PhaseStopped,
		stateInstallFail: cloud.PhaseError,
	} {
		f.hosts["h"] = &uhost.UHostInstanceSet{UHostId: "h", State: state,
			IPSet: []uhost.UHostIPSet{{Type: "Bgp", IP: "1.2.3.4"}, {Type: "Private", IP: "10.0.1.9"}}}
		in, err := d.Get(context.Background(), "h")
		if err != nil || in.Phase != want || in.InternalIP != "10.0.1.9" {
			t.Errorf("state %q: got %+v %v, want phase %s", state, in, err, want)
		}
	}
	if _, err := d.Get(context.Background(), "gone"); !errors.Is(err, cloud.ErrNotFound) {
		t.Errorf("unknown host: want ErrNotFound, got %v", err)
	}
}

// Delete powers a running host off, waits for Stopped, then terminates it with disks and EIP.
func TestDeletePowersOffThenTerminates(t *testing.T) {
	d, f := newDriver()
	f.hosts["h"] = &uhost.UHostInstanceSet{UHostId: "h", State: stateRunning, Zone: "sg-02"}
	ctx := context.Background()

	if err := d.Delete(ctx, "h"); !errors.Is(err, cloud.ErrRetryLater) || len(f.poweroffs) != 1 {
		t.Fatalf("running: want power off + ErrRetryLater, got %v, poweroffs %v", err, f.poweroffs)
	}
	if err := d.Delete(ctx, "h"); !errors.Is(err, cloud.ErrRetryLater) || len(f.poweroffs) != 1 {
		t.Fatalf("stopping: want ErrRetryLater without a second power off, got %v", err)
	}
	f.hosts["h"].State = stateStopped
	if err := d.Delete(ctx, "h"); err != nil {
		t.Fatalf("stopped: want terminate, got %v", err)
	}
	r := f.terminated[0]
	if *r.ReleaseUDisk != true || *r.ReleaseEIP != true || *r.Zone != "sg-02" || *r.Region != "sg" {
		t.Errorf("terminate request %+v", r)
	}
	if err := d.Delete(ctx, "h"); err != nil {
		t.Fatalf("gone: delete must be idempotent, got %v", err)
	}
}

func TestDeleteInstallFailTerminatesAtOnce(t *testing.T) {
	d, f := newDriver()
	f.hosts["h"] = &uhost.UHostInstanceSet{UHostId: "h", State: stateInstallFail, Zone: "sg-02"}
	if err := d.Delete(context.Background(), "h"); err != nil || len(f.terminated) != 1 || len(f.poweroffs) != 0 {
		t.Fatalf("install fail: want terminate without power off, got %v", err)
	}
}

func TestDeleteWhileInitializingWaits(t *testing.T) {
	d, f := newDriver()
	f.hosts["h"] = &uhost.UHostInstanceSet{UHostId: "h", State: stateInitializing}
	if err := d.Delete(context.Background(), "h"); !errors.Is(err, cloud.ErrRetryLater) || len(f.poweroffs)+len(f.terminated) != 0 {
		t.Fatalf("initializing: want ErrRetryLater and no call, got %v", err)
	}
}
