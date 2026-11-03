package workerplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fromControlPlane(t *testing.T) *Template {
	t.Helper()
	b, err := os.ReadFile("testdata/control-plane.json")
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := FromControlPlane(b, []string{"10.0.0.12", "10.0.0.11", "10.0.0.11"})
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestFromControlPlane(t *testing.T) {
	tmpl := fromControlPlane(t)
	if got := strings.Join(tmpl.CPHosts(), ","); got != "10.0.0.11,10.0.0.12" {
		t.Errorf("CP_HOSTS must be the sorted, unique control plane addresses: %s", got)
	}
	cmds, err := tmpl.Commands(Node{Name: "app-ng-abc123", ProviderID: "ucloud://uhost-1", Taints: []string{"debug=true:NoSchedule"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 4 {
		t.Fatalf("want 4 commands, got %d", len(cmds))
	}
	proxy, kubelet := cmds[1], cmds[2]
	for _, want := range []string{
		"docker run -d --name nginx-proxy --network host --restart always",
		"-e CP_HOSTS=10.0.0.11,10.0.0.12",
		"--label io.rancher.rke.container.name=nginx-proxy",
		"--entrypoint nginx-proxy rancher/rke-tools:v0.1.114 CP_HOSTS=10.0.0.11,10.0.0.12",
	} {
		if !strings.Contains(proxy, want) {
			t.Errorf("nginx-proxy lacks %q: %s", want, proxy)
		}
	}
	if strings.Contains(kubelet, "node-role.kubernetes.io") {
		t.Errorf("the control plane role taints must not reach a node group node: %s", kubelet)
	}
	if !strings.Contains(kubelet, "--register-with-taints=debug=true:NoSchedule") {
		t.Errorf("the group's own taints must stay: %s", kubelet)
	}
	if !strings.Contains(kubelet, "--hostname-override=app-ng-abc123") {
		t.Errorf("kubelet keeps the control plane name: %s", kubelet)
	}
}

func TestFromControlPlaneOnlyRoleTaints(t *testing.T) {
	tmpl := fromControlPlane(t)
	cmds, err := tmpl.Commands(Node{Name: "app-ng-abc123", ProviderID: "ucloud://uhost-1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmds[2], "--register-with-taints") {
		t.Errorf("a flag left with only role taints must go: %s", cmds[2])
	}
}

func TestFromControlPlaneRejects(t *testing.T) {
	b, _ := os.ReadFile("testdata/control-plane.json")
	if _, err := FromControlPlane(b, nil); err == nil || !strings.Contains(err.Error(), "CP_HOSTS") {
		t.Errorf("no control plane addresses: %v", err)
	}
	var list []map[string]any
	_ = json.Unmarshal(b, &list)
	noProxy, _ := json.Marshal(list[:2])
	if _, err := FromControlPlane(noProxy, []string{"10.0.0.11"}); err == nil || !strings.Contains(err.Error(), "kube-proxy") {
		t.Errorf("missing kube-proxy: %v", err)
	}
}

func TestDropRoleTaints(t *testing.T) {
	got := dropRoleTaints([]string{"kubelet", "--register-with-taints=node-role.kubernetes.io/controlplane=true:NoSchedule,a=b:NoExecute,node-role.kubernetes.io/etcd:NoExecute", "--v=2"})
	if strings.Join(got, " ") != "kubelet --register-with-taints=a=b:NoExecute --v=2" {
		t.Errorf("got %v", got)
	}
}

// fakeDocker serves GET /containers/{name}/json for the containers of testdata/control-plane.json.
func fakeDocker(t *testing.T, stopped string) string {
	t.Helper()
	b, _ := os.ReadFile("testdata/control-plane.json")
	var list []map[string]any
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, c := range list {
		name := strings.TrimPrefix(c["Name"].(string), "/")
		if name == stopped {
			c["State"] = map[string]any{"Status": "exited", "Running": false}
		}
		byName[name] = c
	}
	dir, err := os.MkdirTemp("", "docker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
		c, ok := byName[name]
		if r.Method != http.MethodGet || !ok {
			http.Error(w, `{"message":"No such container: `+name+`"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(c)
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestInspectLocal(t *testing.T) {
	ctx := context.Background()
	b, err := InspectLocal(ctx, fakeDocker(t, ""), ControlPlaneContainers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromControlPlane(b, []string{"10.0.0.11"}); err != nil {
		t.Errorf("inspect output must build a template: %v", err)
	}
	if _, err := InspectLocal(ctx, fakeDocker(t, "kubelet"), ControlPlaneContainers); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("a stopped kubelet (rke up in progress) must be refused: %v", err)
	}
	if _, err := InspectLocal(ctx, fakeDocker(t, ""), []string{"nginx-proxy"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a missing container must be refused: %v", err)
	}
}
