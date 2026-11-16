package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
clusterName: dev
cloud:
  provider: vngcloud
  vngcloud:
    iamEndpoint: https://iam.example
    vserverEndpoint: https://vserver.example
    projectId: pro-1
bootstrap:
  endpoints: ["https://10.0.0.11:31443"]
nodeGroups:
- name: app
  minSize: 0
  maxSize: 3
  labels: {debug: "true"}
  taints: ["debug=true:NoSchedule"]
  resources: {cpu: "4", memory: 8Gi}
  vngcloud: {imageId: img, flavorId: flav, rootDiskTypeId: vtype, rootDiskSize: 50, networkId: net, subnetId: sub}
`

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := c.Group("app")
	if g.NamePrefix != "dev-app" || g.Resources.Pods.String() != "110" {
		t.Errorf("defaults not applied: %+v", g)
	}
	if c.MaxProvisionTime.Duration.Minutes() != 20 || c.Bootstrap.TokenTTL.Duration.Minutes() != 15 {
		t.Errorf("duration defaults: %v %v", c.MaxProvisionTime, c.Bootstrap.TokenTTL)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, tc := range map[string]struct{ from, to, want string }{
		"kubernetes.io label": {`labels: {debug: "true"}`, `labels: {node-role.kubernetes.io/worker: "true"}`, "kubelet refuses"},
		"reserved label":      {`labels: {debug: "true"}`, `labels: {rke-autoscaler.io/nodegroup: x}`, "set by the provider"},
		"bad taint":           {`"debug=true:NoSchedule"`, `"debug"`, "taint"},
		"min > max":           {`minSize: 0`, `minSize: 5`, "minSize"},
		"http endpoint":       {`https://10.0.0.11`, `http://10.0.0.11`, "must be https"},
		"unknown field":       {`minSize: 0`, "minSize: 0\n  maxSzie: 2", "unknown field"},
		"no resources":        {`resources: {cpu: "4", memory: 8Gi}`, `resources: {}`, "resources"},
		"unsupported cloud":   {`provider: vngcloud`, `provider: aws`, "not supported"},
		"repair percent":      {`minSize: 0`, "minSize: 0\n  repair: {maxUnhealthyPercent: 150}", "maxUnhealthyPercent"},
		"repair too fast":     {`bootstrap:`, "repair: {notReadyAfter: 30s}\nbootstrap:", "at least 1m"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, strings.Replace(valid, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// Fields left unset in a group come from cloud.vngcloud; set ones win; tags merge.
func TestVNGCloudGroupDefaults(t *testing.T) {
	cfg := strings.Replace(valid, "    projectId: pro-1\n", `    projectId: pro-1
    zoneId: HCM03-1A
    imageId: img-global
    flavorId: flav-global
    rootDiskTypeId: vtype-global
    rootDiskSize: 20
    securityGroups: [secg-global]
    sshKeyId: ssh-global
    serverGroupId: sg-global
    tags: {env: dev, team: ops}
`, 1)
	cfg = strings.Replace(cfg,
		"  vngcloud: {imageId: img, flavorId: flav, rootDiskTypeId: vtype, rootDiskSize: 50, networkId: net, subnetId: sub}\n",
		`  vngcloud: {flavorId: flav-big, rootDiskSize: 50, networkId: net, subnetId: sub, tags: {team: app}}
- name: bare
  maxSize: 1
  resources: {cpu: "2", memory: 4Gi}
  vngcloud: {networkId: net, subnetId: sub, securityGroups: []}
`, 1)
	c, err := Load(write(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	app, _ := c.Group("app")
	s := app.VNGCloud
	if s.ImageID != "img-global" || s.FlavorID != "flav-big" || s.RootDiskTypeID != "vtype-global" || s.RootDiskSize != 50 ||
		s.SSHKeyID != "ssh-global" || s.ServerGroupID != "sg-global" || s.ZoneID != "HCM03-1A" ||
		len(s.SecurityGroups) != 1 || s.SecurityGroups[0] != "secg-global" {
		t.Errorf("app: %+v", s)
	}
	if len(s.Tags) != 2 || s.Tags["env"] != "dev" || s.Tags["team"] != "app" {
		t.Errorf("tags must merge, group winning: %v", s.Tags)
	}
	bare, _ := c.Group("bare")
	if bare.VNGCloud.FlavorID != "flav-global" || bare.VNGCloud.SecurityGroups == nil || len(bare.VNGCloud.SecurityGroups) != 0 {
		t.Errorf("bare: explicit securityGroups: [] must stay empty: %+v", bare.VNGCloud)
	}
	if c.Cloud.VNGCloud.Tags["team"] != "ops" {
		t.Error("merging must not modify the cloud-level tags")
	}

	// nothing in the group nor at the cloud level: still rejected
	noImage := strings.Replace(cfg, "    imageId: img-global\n", "", 1)
	if _, err := Load(write(t, noImage)); err == nil || !strings.Contains(err.Error(), "imageId") {
		t.Fatalf("want imageId error, got %v", err)
	}
}

func TestRepairDefaultsAndOverride(t *testing.T) {
	c, err := Load(write(t, strings.Replace(valid, `minSize: 0`, "minSize: 0\n  repair: {enabled: false, notReadyAfter: 15m}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Repair
	if !r.On() || r.NotReadyAfter.Duration != 10*time.Minute || r.VMGoneAfter.Duration != 2*time.Minute ||
		r.MaxUnhealthyPercent != 20 || r.RetryAfter.Duration != 30*time.Minute {
		t.Errorf("defaults: %+v", r)
	}
	g, _ := c.Group("app")
	gr := c.RepairFor(g)
	if gr.On() || gr.NotReadyAfter.Duration != 15*time.Minute || gr.VMGoneAfter.Duration != 2*time.Minute {
		t.Errorf("override: %+v", gr)
	}
}

const validUCloud = `
clusterName: sg
cloud:
  provider: ucloud
  ucloud: {region: sg, projectId: org-x}
bootstrap:
  endpoints: ["https://10.0.0.11:31443"]
nodeGroups:
- name: app
  minSize: 0
  maxSize: 3
  resources: {cpu: "4", memory: 15Gi}
  ucloud:
    zone: sg-02
    imageId: uimage-x
    machineType: O
    cpu: 4
    memoryMB: 16384
    bootDiskType: CLOUD_RSSD
    bootDiskSize: 40
    vpcId: uvnet-x
    subnetId: subnet-x
    secGroupIds: [secgroup-x]
`

func TestLoadUCloud(t *testing.T) {
	c, err := Load(write(t, validUCloud))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := c.Group("app")
	if c.Cloud.UCloud.Region != "sg" || g.UCloud.Zone != "sg-02" || g.UCloud.MemoryMB != 16384 {
		t.Errorf("parsed %+v %+v", c.Cloud.UCloud, g.UCloud)
	}
	for name, tc := range map[string]struct{ from, to, want string }{
		"no region":       {`{region: sg, projectId: org-x}`, `{projectId: org-x}`, "cloud.ucloud needs region"},
		"missing image":   {`imageId: uimage-x`, `imageId: ""`, "ucloud needs zone, imageId"},
		"firewall and sg": {`secGroupIds: [secgroup-x]`, "secGroupIds: [secgroup-x]\n    securityGroupId: firewall-x", "not both"},
		"memory in GB":    {`memoryMB: 16384`, `memoryMB: 16`, "in MB"},
		"bad charge type": {`subnetId: subnet-x`, "subnetId: subnet-x\n    chargeType: Hourly", "chargeType"},
		"too many groups": {`[secgroup-x]`, `[a, b, c, d, e, f]`, "at most 5"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, strings.Replace(validUCloud, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestUCloudGroupDefaults(t *testing.T) {
	cfg := `
clusterName: sg
cloud:
  provider: ucloud
  ucloud:
    region: sg
    zone: sg-02
    imageId: uimage-global
    machineType: "N"
    cpu: 2
    memoryMB: 4096
    bootDiskType: CLOUD_SSD
    bootDiskSize: 20
    vpcId: uvnet-global
    subnetId: subnet-global
    secGroupIds: [secgroup-global]
    keyPairId: uhostkp-global
    tags: {env: dev, team: ops}
bootstrap:
  endpoints: ["https://10.0.0.11:31443"]
nodeGroups:
- name: bare
  maxSize: 1
  resources: {cpu: "2", memory: 3700Mi}
- name: big
  maxSize: 1
  resources: {cpu: "8", memory: 30Gi}
  ucloud: {machineType: O, cpu: 8, memoryMB: 32768, subnetId: subnet-big, tags: {team: app}}
- name: fw
  maxSize: 1
  resources: {cpu: "2", memory: 3700Mi}
  ucloud: {securityGroupId: firewall-x}
`
	c, err := Load(write(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	bare, _ := c.Group("bare")
	if s := bare.UCloud; s == nil || s.Zone != "sg-02" || s.ImageID != "uimage-global" || s.MachineType != "N" ||
		s.CPU != 2 || s.MemoryMB != 4096 || s.BootDiskType != "CLOUD_SSD" || s.BootDiskSize != 20 ||
		s.VPCID != "uvnet-global" || s.SubnetID != "subnet-global" || len(s.SecGroupIDs) != 1 ||
		s.KeyPairID != "uhostkp-global" || len(s.Tags) != 2 {
		t.Errorf("bare must take every default: %+v", s)
	}
	big, _ := c.Group("big")
	if s := big.UCloud; s.MachineType != "O" || s.CPU != 8 || s.MemoryMB != 32768 || s.SubnetID != "subnet-big" ||
		s.ImageID != "uimage-global" || s.Tags["team"] != "app" || s.Tags["env"] != "dev" {
		t.Errorf("big: overrides must win, the rest inherited: %+v", s)
	}
	fw, _ := c.Group("fw")
	if s := fw.UCloud; s.SecurityGroupID != "firewall-x" || s.SecGroupIDs != nil {
		t.Errorf("fw: a group firewall must not be mixed with the default security groups: %+v", s)
	}
	if c.Cloud.UCloud.Tags["team"] != "ops" {
		t.Error("merging must not modify the cloud-level tags")
	}

	noImage := strings.Replace(cfg, "    imageId: uimage-global\n", "", 1)
	if _, err := Load(write(t, noImage)); err == nil || !strings.Contains(err.Error(), "imageId") {
		t.Fatalf("want imageId error, got %v", err)
	}
}

func TestSpecHash(t *testing.T) {
	base := func() NodeGroup {
		return NodeGroup{UCloud: &UCloudServer{Zone: "sg-02", ImageID: "uimage-1", MachineType: "O", CPU: 2, MemoryMB: 8192,
			SubnetID: "subnet-1", SecurityGroupID: "firewall-1", ChargeType: "Dynamic", Tags: map[string]string{"a": "b"}}}
	}
	h := base().SpecHash()
	same := []func(*NodeGroup){
		func(g *NodeGroup) { g.UCloud.Tags = map[string]string{"c": "d"} },
		func(g *NodeGroup) { g.UCloud.ChargeType = "Month" },
		func(g *NodeGroup) { g.UCloud.BusinessGroup = "x" },
		func(g *NodeGroup) { g.UCloud.SecGroupIDs = []string{} },
		func(g *NodeGroup) { g.Labels = map[string]string{"x": "y"} },
	}
	for i, f := range same {
		g := base()
		f(&g)
		if g.SpecHash() != h {
			t.Errorf("change %d must not change the machine spec", i)
		}
	}
	changed := []func(*NodeGroup){
		func(g *NodeGroup) { g.UCloud.MemoryMB = 16384 },
		func(g *NodeGroup) { g.UCloud.ImageID = "uimage-2" },
		func(g *NodeGroup) { g.UCloud.SubnetID = "subnet-2" },
		func(g *NodeGroup) { g.UCloud.BootDiskSize = 40 },
	}
	for i, f := range changed {
		g := base()
		f(&g)
		if g.SpecHash() == h {
			t.Errorf("change %d must change the machine spec", i)
		}
	}
	v1 := NodeGroup{VNGCloud: &VNGCloudServer{FlavorID: "flv-2x8", ImageID: "img"}}
	v2 := NodeGroup{VNGCloud: &VNGCloudServer{FlavorID: "flv-2x16", ImageID: "img"}}
	if v1.SpecHash() == v2.SpecHash() {
		t.Error("vngcloud flavorId must change the machine spec")
	}
}
