// Package workerplane turns `docker inspect` of an existing RKE worker's service-sidekick, nginx-proxy, kubelet
// and kube-proxy into the docker commands that recreate them on a new node.
//
// This is what RKE does for a worker (services/workerplane.go doDeployWorkerPlane), without running `rke up`:
// same images, args, binds and modes; only --hostname-override changes, plus the node group's labels, taints
// and --provider-id on kubelet.
package workerplane

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var Containers = []string{"service-sidekick", "nginx-proxy", "kubelet", "kube-proxy"}

type container struct {
	Name   string `json:"Name"`
	Config struct {
		Image      string            `json:"Image"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode   string   `json:"NetworkMode"`
		PidMode       string   `json:"PidMode"`
		Privileged    bool     `json:"Privileged"`
		Binds         []string `json:"Binds"`
		VolumesFrom   []string `json:"VolumesFrom"`
		SecurityOpt   []string `json:"SecurityOpt"`
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
		LogConfig struct {
			Type   string            `json:"Type"`
			Config map[string]string `json:"Config"`
		} `json:"LogConfig"`
	} `json:"HostConfig"`
}

func (c *container) tokens() []string { return append(append([]string{}, c.Config.Entrypoint...), c.Config.Cmd...) }

func (c *container) flag(name string) string {
	for _, t := range c.tokens() {
		if strings.HasPrefix(t, name+"=") {
			return strings.SplitN(t, "=", 2)[1]
		}
	}
	return ""
}

type Template struct {
	byName       map[string]container
	templateName string // --hostname-override of the node the template was taken from
	templateIP   string // its --node-ip, if RKE set one
}

// Load reads a JSON array as printed by
// `docker inspect service-sidekick nginx-proxy kubelet kube-proxy` on an existing worker.
func Load(path string) (*Template, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Template, error) {
	var list []container
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("worker template is not docker inspect JSON: %w", err)
	}
	t := &Template{byName: map[string]container{}}
	for _, c := range list {
		t.byName[strings.TrimPrefix(c.Name, "/")] = c
	}
	for _, n := range Containers {
		if _, ok := t.byName[n]; !ok {
			return nil, fmt.Errorf("worker template has no %s container", n)
		}
	}
	kubelet := t.byName["kubelet"]
	t.templateName = kubelet.flag("--hostname-override")
	if t.templateName == "" {
		return nil, fmt.Errorf("kubelet in the worker template has no --hostname-override (cloud provider mode is not supported)")
	}
	if tls := kubelet.flag("--tls-cert-file"); strings.Contains(tls, "kube-kubelet") {
		return nil, fmt.Errorf("worker template uses generate_serving_certificate (%s): a new node would need its own "+
			"kubelet serving certificate signed by the cluster CA, which this provider does not issue", tls)
	}
	if kubelet.flag("--provider-id") != "" {
		return nil, fmt.Errorf("kubelet in the worker template already sets --provider-id; take the template from a worker RKE manages")
	}
	t.templateIP = kubelet.flag("--node-ip")
	if len(t.CPHosts()) == 0 {
		return nil, fmt.Errorf("nginx-proxy in the worker template has no CP_HOSTS")
	}
	return t, nil
}

// Images returns the images the worker plane needs, sorted.
func (t *Template) Images() []string {
	set := map[string]bool{}
	for _, n := range Containers {
		set[t.byName[n].Config.Image] = true
	}
	out := make([]string, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	sort.Strings(out)
	return out
}

func (t *Template) KubeletImage() string { return t.byName["kubelet"].Config.Image }

// CPHosts returns the control plane addresses nginx-proxy forwards :6443 to.
func (t *Template) CPHosts() []string {
	for _, e := range t.byName["nginx-proxy"].Config.Env {
		if v, ok := strings.CutPrefix(e, "CP_HOSTS="); ok && v != "" {
			return strings.Split(v, ",")
		}
	}
	return nil
}

type Node struct {
	Name       string
	IP         string // only used when the template has --node-ip
	ProviderID string
	Labels     map[string]string
	Taints     []string
}

// Commands returns the shell commands (already quoted) that start the worker plane, in RKE's order. The sidekick
// is only created: kubelet and kube-proxy use its volumes.
func (t *Template) Commands(n Node) ([]string, error) {
	if n.Name == "" || n.ProviderID == "" {
		return nil, fmt.Errorf("node name and provider ID are required")
	}
	if t.templateIP != "" && n.IP == "" {
		return nil, fmt.Errorf("worker template sets --node-ip, so the node IP is required")
	}
	swap := map[string]string{"--hostname-override=" + t.templateName: "--hostname-override=" + n.Name}
	if t.templateIP != "" {
		swap["--node-ip="+t.templateIP] = "--node-ip=" + n.IP
	}

	cs := map[string]container{}
	for name, c := range t.byName {
		c.Config.Entrypoint = rewrite(c.Config.Entrypoint, swap)
		c.Config.Cmd = rewrite(c.Config.Cmd, swap)
		cs[name] = c
	}

	kubelet := cs["kubelet"]
	if len(n.Labels) > 0 {
		keys := make([]string, 0, len(n.Labels))
		for k := range n.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+n.Labels[k])
		}
		addListFlag(&kubelet, "--node-labels", strings.Join(parts, ","))
	}
	if len(n.Taints) > 0 {
		addListFlag(&kubelet, "--register-with-taints", strings.Join(n.Taints, ","))
	}
	addListFlag(&kubelet, "--provider-id", n.ProviderID)
	cs["kubelet"] = kubelet

	// nothing node-specific of the template node may leak into the new one
	// (whole-word matches only, so a new name that merely starts with the template's name is fine)
	nameRe := regexp.MustCompile(`(^|[^a-z0-9-])` + regexp.QuoteMeta(t.templateName) + `($|[^a-z0-9-])`)
	ipRe := regexp.MustCompile(`(^|[^0-9.])` + regexp.QuoteMeta(t.templateIP) + `($|[^0-9])`)
	for _, name := range Containers {
		c := cs[name]
		for _, tok := range append(c.tokens(), c.Config.Env...) {
			if nameRe.MatchString(tok) || (t.templateIP != "" && ipRe.MatchString(tok)) {
				return nil, fmt.Errorf("%s still references the template node after rewriting: %q", name, tok)
			}
		}
	}

	out := []string{dockerCmd(cs["service-sidekick"], "create")}
	for _, name := range Containers[1:] {
		out = append(out, dockerCmd(cs[name], "run", "-d"))
	}
	return out, nil
}

func rewrite(in []string, swap map[string]string) []string {
	out := make([]string, len(in))
	for i, t := range in {
		if v, ok := swap[t]; ok {
			t = v
		}
		out[i] = t
	}
	return out
}

func addListFlag(c *container, name, value string) {
	lst := &c.Config.Entrypoint
	if len(c.Config.Cmd) > 0 {
		lst = &c.Config.Cmd
	}
	for i, t := range *lst {
		if strings.HasPrefix(t, name+"=") {
			(*lst)[i] = t + "," + value
			return
		}
	}
	*lst = append(*lst, name+"="+value)
}

func dockerCmd(c container, verb ...string) string {
	name := strings.TrimPrefix(c.Name, "/")
	a := append([]string{"docker"}, verb...)
	a = append(a, "--name", name)
	hc := c.HostConfig
	switch hc.NetworkMode {
	case "", "default", "bridge":
	default:
		a = append(a, "--network", hc.NetworkMode)
	}
	if hc.PidMode != "" {
		a = append(a, "--pid", hc.PidMode)
	}
	if hc.Privileged {
		a = append(a, "--privileged")
	}
	if rp := hc.RestartPolicy.Name; rp != "" && rp != "no" {
		a = append(a, "--restart", rp)
	}
	for _, b := range hc.Binds {
		a = append(a, "-v", b)
	}
	for _, v := range hc.VolumesFrom {
		a = append(a, "--volumes-from", v)
	}
	for _, s := range hc.SecurityOpt {
		a = append(a, "--security-opt", s)
	}
	for _, e := range c.Config.Env {
		a = append(a, "-e", e)
	}
	for _, k := range sortedKeys(c.Config.Labels) {
		a = append(a, "--label", k+"="+c.Config.Labels[k])
	}
	if hc.LogConfig.Type != "" {
		a = append(a, "--log-driver", hc.LogConfig.Type)
		for _, k := range sortedKeys(hc.LogConfig.Config) {
			a = append(a, "--log-opt", k+"="+hc.LogConfig.Config[k])
		}
	}
	ep := c.Config.Entrypoint
	if len(ep) > 0 {
		a = append(a, "--entrypoint", ep[0])
	}
	a = append(a, c.Config.Image)
	if len(ep) > 1 {
		a = append(a, ep[1:]...)
	}
	a = append(a, c.Config.Cmd...)
	for i := range a {
		a[i] = Quote(a[i])
	}
	return strings.Join(a, " ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var safeRe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// Quote quotes s for a POSIX shell, like Python's shlex.quote.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if safeRe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
