// Package config loads the provider configuration: cloud settings, bootstrap settings and node groups.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// GroupLabel is set on every node the provider creates. It is how nodes are matched to node groups, and the
// ValidatingAdmissionPolicy shipped with the chart only lets the autoscaler delete nodes carrying it.
const GroupLabel = "rke-autoscaler.io/nodegroup"

type Config struct {
	// ClusterName is written into VM tags so instances of different clusters never get mixed up.
	ClusterName string      `json:"clusterName"`
	Cloud       Cloud       `json:"cloud"`
	Bootstrap   Bootstrap   `json:"bootstrap"`
	NodeGroups  []NodeGroup `json:"nodeGroups"`
	// MaxProvisionTime: an instance that has not registered as a Ready node by then is reported as failed,
	// so cluster-autoscaler deletes it and backs off the group.
	MaxProvisionTime metav1.Duration `json:"maxProvisionTime"`
	// Repair replaces nodes of a group that stopped working; node groups can override single fields.
	Repair Repair `json:"repair"`
}

// Repair: a Running instance whose node is NotReady (or gone) is replaced by a new VM. If its VM still runs,
// the new node is created first and the old one deleted once the new one is Ready; if the VM is gone, stopped
// or in error, both happen at once. See docs/node-repair.md.
type Repair struct {
	Enabled *bool `json:"enabled,omitempty"`
	// NotReadyAfter: how long a node whose VM still runs must be NotReady before it is replaced.
	NotReadyAfter metav1.Duration `json:"notReadyAfter,omitempty"`
	// VMGoneAfter: how long a node whose VM is deleted, stopped or in error must be NotReady before it is
	// replaced.
	VMGoneAfter metav1.Duration `json:"vmGoneAfter,omitempty"`
	// MaxUnhealthyPercent: no repair while more than this share of the group's nodes (or of all nodes of the
	// cluster) is unhealthy; that looks like a network or control plane outage, which new VMs do not fix.
	// One unhealthy node is always repairable.
	MaxUnhealthyPercent int `json:"maxUnhealthyPercent,omitempty"`
	// RetryAfter: after a replacement failed, the next attempt for that node waits this long.
	RetryAfter metav1.Duration `json:"retryAfter,omitempty"`
}

func (r Repair) On() bool { return r.Enabled == nil || *r.Enabled }

// merge returns r with the fields set in o replacing its own.
func (r Repair) merge(o *Repair) Repair {
	if o == nil {
		return r
	}
	if o.Enabled != nil {
		r.Enabled = o.Enabled
	}
	if o.NotReadyAfter.Duration != 0 {
		r.NotReadyAfter = o.NotReadyAfter
	}
	if o.VMGoneAfter.Duration != 0 {
		r.VMGoneAfter = o.VMGoneAfter
	}
	if o.MaxUnhealthyPercent != 0 {
		r.MaxUnhealthyPercent = o.MaxUnhealthyPercent
	}
	if o.RetryAfter.Duration != 0 {
		r.RetryAfter = o.RetryAfter
	}
	return r
}

type Cloud struct {
	Provider string    `json:"provider"`
	VNGCloud *VNGCloud `json:"vngcloud,omitempty"`
	UCloud   *UCloud   `json:"ucloud,omitempty"`
}

// VNGCloud holds account-level settings. Credentials come from the environment (VNGCLOUD_CLIENT_ID /
// VNGCLOUD_CLIENT_SECRET), never from this file.
type VNGCloud struct {
	IAMEndpoint     string `json:"iamEndpoint"`
	VServerEndpoint string `json:"vserverEndpoint"`
	ProjectID       string `json:"projectId"`
	ZoneID          string `json:"zoneId,omitempty"`

	// Defaults for every node group: a group's vngcloud block takes each of these it leaves unset (empty
	// string, 0, or securityGroups absent). Tags are merged, the group's value winning on the same key.
	ImageID        string            `json:"imageId,omitempty"`
	FlavorID       string            `json:"flavorId,omitempty"`
	RootDiskTypeID string            `json:"rootDiskTypeId,omitempty"`
	RootDiskSize   int               `json:"rootDiskSize,omitempty"`
	SecurityGroups []string          `json:"securityGroups,omitempty"`
	SSHKeyID       string            `json:"sshKeyId,omitempty"`
	ServerGroupID  string            `json:"serverGroupId,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
}

// UCloud holds account-level settings. Credentials come from the environment (UCLOUD_PUBLIC_KEY /
// UCLOUD_PRIVATE_KEY), never from this file.
type UCloud struct {
	// Region, e.g. sg (Singapore).
	Region string `json:"region"`
	// ProjectID, e.g. org-xxxx; required for sub-accounts.
	ProjectID string `json:"projectId,omitempty"`
	// APIEndpoint defaults to https://api.ucloud.cn, which serves every region.
	APIEndpoint string `json:"apiEndpoint,omitempty"`

	// Defaults for every node group: a group's ucloud block takes each field it leaves unset (empty string,
	// 0, or the security fields absent). Tags are merged, the group's value winning on the same key.
	UCloudServer
}

type Bootstrap struct {
	// Endpoints are the URLs a new VM tries, in order, to fetch its join script, e.g. the bootstrap NodePort
	// on the existing nodes: https://10.0.0.11:31443. The VM is not in the cluster yet, so it cannot use a
	// Service name.
	Endpoints []string `json:"endpoints"`
	// TokenTTL bounds how long a VM's bootstrap token stays valid after the VM is created.
	TokenTTL metav1.Duration `json:"tokenTTL"`
	// PreJoinScript runs on the VM before the join, e.g. to install docker when the image does not ship it.
	PreJoinScript string `json:"preJoinScript,omitempty"`
}

type NodeGroup struct {
	Name    string `json:"name"`
	MinSize int    `json:"minSize"`
	MaxSize int    `json:"maxSize"`
	// NamePrefix of VM and node names; a random suffix is appended. Defaults to "<clusterName>-<name>".
	NamePrefix string            `json:"namePrefix,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// Taints as key=value:Effect or key:Effect, registered by kubelet (--register-with-taints).
	Taints []string `json:"taints,omitempty"`
	// Resources of one VM of this group, used for the template node cluster-autoscaler simulates when the
	// group has no node yet (scale from zero). Keep them equal to what the flavor really gives a node.
	Resources Resources       `json:"resources"`
	VNGCloud  *VNGCloudServer `json:"vngcloud,omitempty"`
	UCloud    *UCloudServer   `json:"ucloud,omitempty"`
	// Repair overrides fields of the top-level repair settings for this group.
	Repair *Repair `json:"repair,omitempty"`
}

type Resources struct {
	CPU              resource.Quantity `json:"cpu"`
	Memory           resource.Quantity `json:"memory"`
	Pods             resource.Quantity `json:"pods"`
	EphemeralStorage resource.Quantity `json:"ephemeralStorage"`
}

type VNGCloudServer struct {
	ImageID        string            `json:"imageId"`
	FlavorID       string            `json:"flavorId"`
	RootDiskTypeID string            `json:"rootDiskTypeId"`
	RootDiskSize   int               `json:"rootDiskSize"`
	NetworkID      string            `json:"networkId"`
	SubnetID       string            `json:"subnetId"`
	SecurityGroups []string          `json:"securityGroups,omitempty"`
	SSHKeyID       string            `json:"sshKeyId,omitempty"`
	ServerGroupID  string            `json:"serverGroupId,omitempty"`
	ZoneID         string            `json:"zoneId,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
}

// UCloudServer describes the UHost of a node group (CreateUHostInstance parameters).
type UCloudServer struct {
	// Zone, e.g. sg-01. Image ids are per zone.
	Zone    string `json:"zone"`
	ImageID string `json:"imageId"`
	// MachineType N, C, O, OM, ... with CPU cores and memory in MB, e.g. O / 4 / 16384.
	MachineType string `json:"machineType"`
	// MinimalCPUPlatform, e.g. Intel/Auto or Amd/Auto (optional).
	MinimalCPUPlatform string `json:"minimalCpuPlatform,omitempty"`
	CPU                int    `json:"cpu"`
	MemoryMB           int    `json:"memoryMB"`
	// BootDiskType CLOUD_SSD, CLOUD_RSSD, LOCAL_NORMAL, ...; size in GB.
	BootDiskType string `json:"bootDiskType"`
	BootDiskSize int    `json:"bootDiskSize"`
	VPCID        string `json:"vpcId"`
	SubnetID     string `json:"subnetId"`
	// Either SecurityGroupID (a firewall, "Firewall" security mode) or SecGroupIDs (up to 5 security groups,
	// "SecGroup" mode, in priority order).
	SecurityGroupID string   `json:"securityGroupId,omitempty"`
	SecGroupIDs     []string `json:"secGroupIds,omitempty"`
	// KeyPairID: log in with this key pair. Without it the host gets a random password nobody keeps; reach it
	// through the SSH keys of bootstrap.cloudConfig.
	KeyPairID string `json:"keyPairId,omitempty"`
	// ChargeType defaults to Dynamic (hourly): node group hosts live for hours, not months.
	ChargeType     string `json:"chargeType,omitempty"`
	IsolationGroup string `json:"isolationGroup,omitempty"`
	// BusinessGroup is the UHost "Tag" (business group), Default if empty.
	BusinessGroup string            `json:"businessGroup,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
}

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	taintRe = regexp.MustCompile(`^[A-Za-z0-9./_-]+(=[A-Za-z0-9._-]*)?:(NoSchedule|PreferNoSchedule|NoExecute)$`)
)

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.MaxProvisionTime.Duration == 0 {
		c.MaxProvisionTime.Duration = 20 * time.Minute
	}
	if c.Bootstrap.TokenTTL.Duration == 0 {
		c.Bootstrap.TokenTTL.Duration = 15 * time.Minute
	}
	r := &c.Repair
	if r.NotReadyAfter.Duration == 0 {
		r.NotReadyAfter.Duration = 10 * time.Minute
	}
	if r.VMGoneAfter.Duration == 0 {
		r.VMGoneAfter.Duration = 2 * time.Minute
	}
	if r.MaxUnhealthyPercent == 0 {
		r.MaxUnhealthyPercent = 20
	}
	if r.RetryAfter.Duration == 0 {
		r.RetryAfter.Duration = 30 * time.Minute
	}
	for i := range c.NodeGroups {
		g := &c.NodeGroups[i]
		if g.NamePrefix == "" {
			g.NamePrefix = c.ClusterName + "-" + g.Name
		}
		if g.Resources.Pods.IsZero() {
			g.Resources.Pods = resource.MustParse("110")
		}
		if v := c.Cloud.VNGCloud; c.Cloud.Provider == "vngcloud" && v != nil {
			if g.VNGCloud == nil {
				g.VNGCloud = &VNGCloudServer{}
			}
			g.VNGCloud.inherit(v)
		}
		if u := c.Cloud.UCloud; c.Cloud.Provider == "ucloud" && u != nil {
			if g.UCloud == nil {
				g.UCloud = &UCloudServer{}
			}
			g.UCloud.inherit(&u.UCloudServer)
		}
	}
}

// inherit fills the fields a group left unset from the cloud-level defaults. An explicit empty
// securityGroups list ([]) is kept: it means no security group, not "use the default".
func (s *VNGCloudServer) inherit(d *VNGCloud) {
	str := func(f *string, def string) {
		if *f == "" {
			*f = def
		}
	}
	str(&s.ImageID, d.ImageID)
	str(&s.FlavorID, d.FlavorID)
	str(&s.RootDiskTypeID, d.RootDiskTypeID)
	str(&s.SSHKeyID, d.SSHKeyID)
	str(&s.ServerGroupID, d.ServerGroupID)
	str(&s.ZoneID, d.ZoneID)
	if s.RootDiskSize == 0 {
		s.RootDiskSize = d.RootDiskSize
	}
	if s.SecurityGroups == nil {
		s.SecurityGroups = d.SecurityGroups
	}
	s.Tags = mergeTags(d.Tags, s.Tags)
}

// inherit fills the fields a group left unset from the cloud-level defaults. The security setting is taken
// as a whole, and only when the group sets neither securityGroupId nor secGroupIds: mixing a firewall from
// one level with security groups from the other would be rejected. An explicit secGroupIds: [] is kept.
func (s *UCloudServer) inherit(d *UCloudServer) {
	str := func(f *string, def string) {
		if *f == "" {
			*f = def
		}
	}
	num := func(f *int, def int) {
		if *f == 0 {
			*f = def
		}
	}
	str(&s.Zone, d.Zone)
	str(&s.ImageID, d.ImageID)
	str(&s.MachineType, d.MachineType)
	str(&s.MinimalCPUPlatform, d.MinimalCPUPlatform)
	num(&s.CPU, d.CPU)
	num(&s.MemoryMB, d.MemoryMB)
	str(&s.BootDiskType, d.BootDiskType)
	num(&s.BootDiskSize, d.BootDiskSize)
	str(&s.VPCID, d.VPCID)
	str(&s.SubnetID, d.SubnetID)
	if s.SecurityGroupID == "" && s.SecGroupIDs == nil {
		s.SecurityGroupID, s.SecGroupIDs = d.SecurityGroupID, d.SecGroupIDs
	}
	str(&s.KeyPairID, d.KeyPairID)
	str(&s.ChargeType, d.ChargeType)
	str(&s.IsolationGroup, d.IsolationGroup)
	str(&s.BusinessGroup, d.BusinessGroup)
	s.Tags = mergeTags(d.Tags, s.Tags)
}

// mergeTags returns the defaults overlaid with the group's tags, in a new map when there are defaults.
func mergeTags(defaults, group map[string]string) map[string]string {
	if len(defaults) == 0 {
		return group
	}
	tags := make(map[string]string, len(defaults)+len(group))
	for k, v := range defaults {
		tags[k] = v
	}
	for k, v := range group {
		tags[k] = v
	}
	return tags
}

func (c *Config) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if !nameRe.MatchString(c.ClusterName) {
		add("clusterName %q must be a lowercase DNS label", c.ClusterName)
	}
	switch c.Cloud.Provider {
	case "vngcloud":
		v := c.Cloud.VNGCloud
		if v == nil || v.IAMEndpoint == "" || v.VServerEndpoint == "" || v.ProjectID == "" {
			add("cloud.vngcloud needs iamEndpoint, vserverEndpoint and projectId")
		}
	case "ucloud":
		if u := c.Cloud.UCloud; u == nil || u.Region == "" {
			add("cloud.ucloud needs region")
		}
	default:
		add("cloud.provider %q is not supported (supported: vngcloud, ucloud)", c.Cloud.Provider)
	}
	if len(c.Bootstrap.Endpoints) == 0 {
		add("bootstrap.endpoints must list at least one URL reachable from new VMs")
	}
	for _, e := range c.Bootstrap.Endpoints {
		if !strings.HasPrefix(e, "https://") {
			add("bootstrap endpoint %q must be https", e)
		}
	}
	if c.Bootstrap.TokenTTL.Duration > c.MaxProvisionTime.Duration {
		add("bootstrap.tokenTTL (%s) must not exceed maxProvisionTime (%s)", c.Bootstrap.TokenTTL.Duration, c.MaxProvisionTime.Duration)
	}

	checkRepair := func(where string, r Repair) {
		if r.MaxUnhealthyPercent < 1 || r.MaxUnhealthyPercent > 100 {
			add("%srepair.maxUnhealthyPercent must be 1..100, got %d", where, r.MaxUnhealthyPercent)
		}
		// below ~1m the node controller has not even marked a silent kubelet NotReady (40s grace)
		if r.NotReadyAfter.Duration < time.Minute || r.VMGoneAfter.Duration < time.Minute {
			add("%srepair.notReadyAfter and repair.vmGoneAfter must be at least 1m", where)
		}
	}
	checkRepair("", c.Repair)

	seen := map[string]bool{}
	for _, g := range c.NodeGroups {
		if g.Repair != nil {
			checkRepair("node group "+g.Name+": ", c.RepairFor(g))
		}
		if !nameRe.MatchString(g.Name) {
			add("node group name %q must be a lowercase DNS label", g.Name)
		}
		if seen[g.Name] {
			add("duplicate node group %q", g.Name)
		}
		seen[g.Name] = true
		if g.MinSize < 0 || g.MaxSize < g.MinSize {
			add("node group %s: need 0 <= minSize <= maxSize", g.Name)
		}
		if !nameRe.MatchString(g.NamePrefix) || len(g.NamePrefix) > 50 {
			add("node group %s: namePrefix %q must be a lowercase DNS label of at most 50 chars", g.Name, g.NamePrefix)
		}
		for k := range g.Labels {
			// kubelet refuses these in --node-labels (except a small allow-list we do not try to mirror)
			if strings.Contains(k, "kubernetes.io/") || strings.Contains(k, "k8s.io/") {
				add("node group %s: label %q uses a kubernetes.io/k8s.io prefix, which kubelet refuses", g.Name, k)
			}
			if k == GroupLabel {
				add("node group %s: label %q is set by the provider", g.Name, k)
			}
		}
		for _, t := range g.Taints {
			if !taintRe.MatchString(t) {
				add("node group %s: taint %q must be key=value:Effect or key:Effect", g.Name, t)
			}
		}
		if g.Resources.CPU.IsZero() || g.Resources.Memory.IsZero() {
			add("node group %s: resources.cpu and resources.memory are required", g.Name)
		}
		if c.Cloud.Provider == "vngcloud" {
			s := g.VNGCloud
			if s == nil || s.ImageID == "" || s.FlavorID == "" || s.RootDiskTypeID == "" || s.RootDiskSize == 0 ||
				s.NetworkID == "" || s.SubnetID == "" {
				add("node group %s: vngcloud needs imageId, flavorId, rootDiskTypeId, rootDiskSize (in the group or in cloud.vngcloud), networkId, subnetId", g.Name)
			}
		}
		if c.Cloud.Provider == "ucloud" {
			validateUCloudServer(g.Name, g.UCloud, add)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// RepairFor returns the repair settings of a group: the top-level ones with the group's overrides applied.
func (c *Config) RepairFor(g NodeGroup) Repair { return c.Repair.merge(g.Repair) }

func (c *Config) Group(name string) (NodeGroup, bool) {
	for _, g := range c.NodeGroups {
		if g.Name == name {
			return g, true
		}
	}
	return NodeGroup{}, false
}

var ucloudChargeTypes = map[string]bool{"": true, "Dynamic": true, "Postpay": true, "Month": true, "Year": true}

func validateUCloudServer(group string, s *UCloudServer, add func(string, ...any)) {
	if s == nil || s.Zone == "" || s.ImageID == "" || s.MachineType == "" || s.CPU <= 0 || s.MemoryMB <= 0 ||
		s.BootDiskType == "" || s.BootDiskSize <= 0 || s.VPCID == "" || s.SubnetID == "" {
		add("node group %s: ucloud needs zone, imageId, machineType, cpu, memoryMB, bootDiskType, bootDiskSize, vpcId, subnetId (in the group or in cloud.ucloud)", group)
		return
	}
	if s.SecurityGroupID != "" && len(s.SecGroupIDs) > 0 {
		add("node group %s: ucloud takes securityGroupId (firewall) or secGroupIds (security groups), not both", group)
	}
	if len(s.SecGroupIDs) > 5 {
		add("node group %s: ucloud secGroupIds takes at most 5 security groups", group)
	}
	if !ucloudChargeTypes[s.ChargeType] {
		add("node group %s: ucloud chargeType %q must be Dynamic, Postpay, Month or Year", group, s.ChargeType)
	}
	if s.MemoryMB < 1024 {
		add("node group %s: ucloud memoryMB is in MB (e.g. 16384), got %d", group, s.MemoryMB)
	}
}
