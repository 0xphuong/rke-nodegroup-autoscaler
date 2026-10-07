package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, strings.Replace(valid, tc.from, tc.to, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
