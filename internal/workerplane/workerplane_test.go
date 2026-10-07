package workerplane

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T) *Template {
	t.Helper()
	tmpl, err := Load("testdata/worker-template.json")
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func TestCommands(t *testing.T) {
	tmpl := load(t)
	cmds, err := tmpl.Commands(Node{
		Name:       "app-ng-abc123",
		ProviderID: "vngcloud://ins-1",
		Labels:     map[string]string{"rke-autoscaler.io/nodegroup": "app", "debug": "true"},
		Taints:     []string{"debug=true:NoSchedule"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 4 {
		t.Fatalf("want 4 commands, got %d", len(cmds))
	}
	if !strings.HasPrefix(cmds[0], "docker create --name service-sidekick ") {
		t.Errorf("sidekick must only be created: %s", cmds[0])
	}
	for i, name := range []string{"nginx-proxy", "kubelet", "kube-proxy"} {
		if !strings.HasPrefix(cmds[i+1], "docker run -d --name "+name+" ") {
			t.Errorf("command %d: want %s, got %.60s", i+1, name, cmds[i+1])
		}
	}
	kubelet, proxy := cmds[2], cmds[3]
	for _, want := range []string{
		"--hostname-override=app-ng-abc123",
		"--provider-id=vngcloud://ins-1",
		"--node-labels=debug=true,rke-autoscaler.io/nodegroup=app",
		"--register-with-taints=debug=true:NoSchedule",
		"--volumes-from service-sidekick",
	} {
		if !strings.Contains(kubelet, want) {
			t.Errorf("kubelet command lacks %q", want)
		}
	}
	if !strings.Contains(proxy, "--hostname-override=app-ng-abc123") {
		t.Errorf("kube-proxy keeps the template name: %s", proxy)
	}
	for _, c := range cmds {
		if strings.Contains(c, "worker-0 ") || strings.HasSuffix(c, "worker-0") || strings.Contains(c, "=worker-0") {
			t.Errorf("template node name leaked: %s", c)
		}
	}
}

func TestNewNameMayStartWithTemplateName(t *testing.T) {
	if _, err := load(t).Commands(Node{Name: "worker-0-x1", ProviderID: "vngcloud://i"}); err != nil {
		t.Fatalf("a name that starts with the template's must be allowed: %v", err)
	}
}

func TestTemplateFacts(t *testing.T) {
	tmpl := load(t)
	if got := strings.Join(tmpl.CPHosts(), ","); got != "10.0.0.11,10.0.0.12,10.0.0.13" {
		t.Errorf("CPHosts = %s", got)
	}
	if len(tmpl.Images()) != 2 {
		t.Errorf("Images = %v", tmpl.Images())
	}
	if !strings.HasPrefix(tmpl.KubeletImage(), "rancher/hyperkube:") {
		t.Errorf("KubeletImage = %s", tmpl.KubeletImage())
	}
}

func TestRejectsServingCertificateTemplates(t *testing.T) {
	b, err := os.ReadFile("testdata/worker-template.json")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), `"--client-ca-file=`, `"--tls-cert-file=/etc/kubernetes/ssl/kube-kubelet-10-0-0-1.pem", "--client-ca-file=`, 1)
	if _, err := Parse([]byte(s)); err == nil || !strings.Contains(err.Error(), "generate_serving_certificate") {
		t.Fatalf("want generate_serving_certificate error, got %v", err)
	}
}

func TestRejectsIncompleteTemplate(t *testing.T) {
	if _, err := Parse([]byte(`[{"Name":"/kubelet"}]`)); err == nil {
		t.Fatal("want error for a template without all four containers")
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                "plain",
		"a b":                  "'a b'",
		"it's":                 `'it'"'"'s'`,
		"":                     "''",
		"--opt=a,b":            "--opt=a,b",
		"maintainer=X <y@z.c>": "'maintainer=X <y@z.c>'",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
