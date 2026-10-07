package preflight

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/bootstrap"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

type issuer struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
}

func newCA(t *testing.T) issuer {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kube-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	c, _ := x509.ParseCertificate(der)
	return issuer{c, key}
}

func (ca issuer) client(t *testing.T, cn string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func (ca issuer) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

func bundle(t *testing.T, ca issuer, nodeNotAfter time.Time) bootstrap.Bundle {
	t.Helper()
	nc, nk := ca.client(t, "system:node", nodeNotAfter)
	pc, pk := ca.client(t, "system:kube-proxy", time.Now().Add(365*24*time.Hour))
	kc := []byte("clusters:\n- cluster:\n    server: \"https://127.0.0.1:6443\"\n")
	return bootstrap.Bundle{"kube-ca.pem": ca.pem(), "kube-node.pem": nc, "kube-node-key.pem": nk,
		"kube-proxy.pem": pc, "kube-proxy-key.pem": pk, "kubecfg-kube-node.yaml": kc, "kubecfg-kube-proxy.yaml": kc}
}

func TestBundle(t *testing.T) {
	ca := newCA(t)
	var warnings []string
	warn := func(s string) { warnings = append(warnings, s) }
	if err := Bundle(bundle(t, ca, time.Now().Add(365*24*time.Hour)), time.Now(), 30*24*time.Hour, warn); err != nil || len(warnings) != 0 {
		t.Fatalf("valid bundle: err %v warnings %v", err, warnings)
	}
	if err := Bundle(bundle(t, ca, time.Now().Add(24*time.Hour)), time.Now(), 30*24*time.Hour, warn); err != nil || len(warnings) != 1 {
		t.Fatalf("soon-expiring cert must warn, not fail: err %v warnings %v", err, warnings)
	}

	b := bundle(t, ca, time.Now().Add(365*24*time.Hour))
	b["kube-ca.pem"] = newCA(t).pem()
	if err := Bundle(b, time.Now(), 0, warn); err == nil {
		t.Fatal("certs of another CA must be refused")
	}

	b = bundle(t, ca, time.Now().Add(365*24*time.Hour))
	b["kube-node-key.pem"] = b["kube-proxy-key.pem"]
	if err := Bundle(b, time.Now(), 0, warn); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("swapped key must be refused, got %v", err)
	}

	b = bundle(t, ca, time.Now().Add(365*24*time.Hour))
	b["kubecfg-kube-node.yaml"] = []byte("server: https://10.0.0.11:6443")
	if err := Bundle(b, time.Now(), 0, warn); err == nil {
		t.Fatal("a kubeconfig not pointing at nginx-proxy must be refused")
	}
}

func TestClusterCA(t *testing.T) {
	ca := newCA(t)
	b := bundle(t, ca, time.Now().Add(time.Hour))
	p := filepath.Join(t.TempDir(), "ca.crt")
	_ = os.WriteFile(p, append(newCA(t).pem(), ca.pem()...), 0o600)
	if err := ClusterCA(b, p); err != nil {
		t.Fatalf("CA present in the service account bundle: %v", err)
	}
	_ = os.WriteFile(p, newCA(t).pem(), 0o600)
	if err := ClusterCA(b, p); err == nil {
		t.Fatal("foreign CA must be refused")
	}
}

func TestTemplate(t *testing.T) {
	tmpl, err := workerplane.Load("../workerplane/testdata/worker-template.json")
	if err != nil {
		t.Fatal(err)
	}
	node := func(name, version, ip string, cp bool) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: version},
				Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}}}}
		if cp {
			n.Labels["node-role.kubernetes.io/controlplane"] = "true"
		}
		return n
	}
	version := strings.TrimSuffix(strings.SplitN(tmpl.KubeletImage(), ":", 2)[1], "-rancher1")
	ok := fake.NewSimpleClientset(node("cp-0", version, "10.0.0.11", true), node("cp-1", version, "10.0.0.12", true),
		node("cp-2", version, "10.0.0.13", true), node("w-0", version, "10.0.0.20", false))
	if err := Template(context.Background(), ok, tmpl); err != nil {
		t.Fatalf("matching cluster: %v", err)
	}
	upgraded := fake.NewSimpleClientset(node("cp-0", "v1.33.1", "10.0.0.11", true))
	if err := Template(context.Background(), upgraded, tmpl); err == nil || !strings.Contains(err.Error(), "re-take") {
		t.Fatalf("after an upgrade the template must be refused, got %v", err)
	}
	cpChanged := fake.NewSimpleClientset(node("cp-0", version, "10.0.0.11", true), node("cp-9", version, "10.0.0.99", true))
	if err := Template(context.Background(), cpChanged, tmpl); err == nil || !strings.Contains(err.Error(), "CP_HOSTS") {
		t.Fatalf("changed control planes must be refused, got %v", err)
	}
}
