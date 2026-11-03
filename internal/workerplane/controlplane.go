package workerplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// ControlPlaneContainers are the worker plane containers RKE also runs on a control plane node. nginx-proxy is
// not among them: RKE runs it only on nodes without the controlplane role, so FromControlPlane builds it.
var ControlPlaneContainers = []string{"service-sidekick", "kubelet", "kube-proxy"}

// roleTaints are the taints RKE registers for a node's own roles; a node group node has neither role.
var roleTaints = []string{"node-role.kubernetes.io/controlplane", "node-role.kubernetes.io/etcd"}

// FromControlPlane builds the template from `docker inspect service-sidekick kubelet kube-proxy` of a control
// plane node. A control plane kubelet differs from a worker's only by its role taints
// (--register-with-taints), which are dropped; nginx-proxy is built the way RKE builds it
// (cluster/plan.go BuildProxyProcess): the rke-tools image of the sidekick, host network, CP_HOSTS = the control
// plane addresses.
func FromControlPlane(b []byte, cpHosts []string) (*Template, error) {
	var list []container
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("control plane containers are not docker inspect JSON: %w", err)
	}
	byName := map[string]container{}
	for _, c := range list {
		byName[strings.TrimPrefix(c.Name, "/")] = c
	}
	for _, n := range ControlPlaneContainers {
		if _, ok := byName[n]; !ok {
			return nil, fmt.Errorf("control plane node has no %s container", n)
		}
	}
	hosts := uniqueSorted(cpHosts)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no control plane addresses for nginx-proxy's CP_HOSTS")
	}

	kubelet := byName["kubelet"]
	kubelet.Config.Entrypoint = dropRoleTaints(kubelet.Config.Entrypoint)
	kubelet.Config.Cmd = dropRoleTaints(kubelet.Config.Cmd)

	env := "CP_HOSTS=" + strings.Join(hosts, ",")
	proxy := container{Name: "/nginx-proxy"}
	proxy.Config.Image = byName["service-sidekick"].Config.Image
	proxy.Config.Entrypoint = []string{"nginx-proxy"}
	// RKE passes CP_HOSTS as the argument too, to recreate the container when the control plane changes
	proxy.Config.Cmd = []string{env}
	proxy.Config.Env = []string{env}
	proxy.Config.Labels = map[string]string{"io.rancher.rke.container.name": "nginx-proxy"}
	proxy.HostConfig.NetworkMode = "host"
	proxy.HostConfig.RestartPolicy.Name = "always"
	proxy.HostConfig.LogConfig = kubelet.HostConfig.LogConfig

	out, err := json.Marshal([]container{byName["service-sidekick"], proxy, kubelet, byName["kube-proxy"]})
	if err != nil {
		return nil, err
	}
	return Parse(out)
}

// dropRoleTaints removes the role taints from --register-with-taints, and the flag when nothing is left.
func dropRoleTaints(tokens []string) []string {
	out := tokens[:0:0]
	for _, t := range tokens {
		v, ok := strings.CutPrefix(t, "--register-with-taints=")
		if !ok {
			out = append(out, t)
			continue
		}
		var keep []string
		for _, taint := range strings.Split(v, ",") {
			key := strings.SplitN(strings.SplitN(taint, ":", 2)[0], "=", 2)[0]
			role := false
			for _, r := range roleTaints {
				role = role || key == r
			}
			if taint != "" && !role {
				keep = append(keep, taint)
			}
		}
		if len(keep) > 0 {
			out = append(out, "--register-with-taints="+strings.Join(keep, ","))
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		if s != "" {
			set[s] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// InspectLocal returns `docker inspect` of the named containers as one JSON array, read from the Docker Engine
// API on a unix socket (GET /containers/{name}/json only). Every container must exist; all but
// service-sidekick, which RKE only creates, must be running.
func InspectLocal(ctx context.Context, socket string, names []string) ([]byte, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	var all []json.RawMessage
	for _, name := range names {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(name)+"/json", nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", name, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", name, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("docker inspect %s: %s: %s", name, resp.Status, strings.TrimSpace(string(body)))
		}
		var st struct {
			State struct {
				Running bool `json:"Running"`
			} `json:"State"`
		}
		if err := json.Unmarshal(body, &st); err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", name, err)
		}
		if name != "service-sidekick" && !st.State.Running {
			return nil, fmt.Errorf("container %s is not running", name)
		}
		all = append(all, body)
	}
	return json.Marshal(all)
}
