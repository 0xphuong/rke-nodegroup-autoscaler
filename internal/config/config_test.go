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
