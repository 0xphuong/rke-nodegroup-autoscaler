// Package preflight refuses to start the provider with inputs that would produce broken nodes: a certificate
// bundle from another cluster, or a worker template that no longer matches the cluster after an `rke up`.
package preflight

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/bootstrap"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

// Bundle checks the node certificates: kube-node and kube-proxy are signed by kube-ca.pem, each key belongs to
// its certificate, and nothing expires within warnWithin (reported through warn, not as an error).
func Bundle(b bootstrap.Bundle, now time.Time, warnWithin time.Duration, warn func(string)) error {
	ca, err := parseCert(b["kube-ca.pem"])
	if err != nil {
		return fmt.Errorf("kube-ca.pem: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, name := range []string{"kube-node", "kube-proxy"} {
		cert, err := parseCert(b[name+".pem"])
		if err != nil {
			return fmt.Errorf("%s.pem: %w", name, err)
		}
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			return fmt.Errorf("%s.pem is not a valid client certificate of kube-ca.pem: %w", name, err)
		}
		if err := keyMatches(cert, b[name+"-key.pem"]); err != nil {
			return fmt.Errorf("%s-key.pem: %w", name, err)
		}
		if left := cert.NotAfter.Sub(now); left < warnWithin {
			warn(fmt.Sprintf("%s.pem expires %s (in %s)", name, cert.NotAfter.Format(time.RFC3339), left.Round(time.Hour)))
		}
	}
	for _, f := range []string{"kubecfg-kube-node.yaml", "kubecfg-kube-proxy.yaml"} {
		if !bytes.Contains(b[f], []byte("https://127.0.0.1:6443")) {
			return fmt.Errorf("%s does not point at the local nginx-proxy (https://127.0.0.1:6443); take it from an RKE worker", f)
		}
	}
	return nil
}

// ClusterCA checks that kube-ca.pem of the bundle is the CA of the cluster the provider runs in (the service
// account ca.crt; RKE uses kube-ca for both).
func ClusterCA(b bootstrap.Bundle, saCAPath string) error {
	saCA, err := os.ReadFile(saCAPath)
	if err != nil {
		return err
	}
	want, err := parseCert(b["kube-ca.pem"])
	if err != nil {
		return err
	}
	for rest := saCA; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type == "CERTIFICATE" && bytes.Equal(blk.Bytes, want.Raw) {
			return nil
		}
	}
	return errors.New("kube-ca.pem of the node certificate Secret is not this cluster's CA — certificates copied from another cluster?")
}

// Template checks that the worker template still matches the cluster: same Kubernetes version as the nodes,
// same control plane addresses as nginx-proxy's CP_HOSTS. Both change with `rke up` (upgrade, control plane
// added or removed), after which the template has to be taken again.
func Template(ctx context.Context, kube kubernetes.Interface, tmpl *workerplane.Template) error {
	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "!" + config.GroupLabel})
	if err != nil {
		return err
	}
	versions := map[string]bool{}
	var cps []string
	for _, n := range nodes.Items {
		versions[n.Status.NodeInfo.KubeletVersion] = true
		if n.Labels["node-role.kubernetes.io/controlplane"] == "true" {
			for _, a := range n.Status.Addresses {
				if a.Type == "InternalIP" {
					cps = append(cps, a.Address)
				}
			}
		}
	}
	tag := tmpl.KubeletImage()
	if i := strings.LastIndex(tag, ":"); i >= 0 {
		tag = tag[i+1:]
	}
	matched := false
	for v := range versions {
		if v != "" && (tag == v || strings.HasPrefix(tag, v+"-")) {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("worker template kubelet image %s does not match the cluster's kubelet versions %v — re-take the template", tmpl.KubeletImage(), keys(versions))
	}
	if len(cps) > 0 {
		have := append([]string{}, tmpl.CPHosts()...)
		sort.Strings(have)
		sort.Strings(cps)
		if strings.Join(have, ",") != strings.Join(cps, ",") {
			return fmt.Errorf("worker template CP_HOSTS %v differ from the control plane nodes %v — re-take the template", have, cps)
		}
	}
	return nil
}

func parseCert(p []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(p)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func keyMatches(cert *x509.Certificate, keyPEM []byte) error {
	blk, _ := pem.Decode(keyPEM)
	if blk == nil {
		return errors.New("no PEM key")
	}
	var key any
	var err error
	switch blk.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(blk.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(blk.Bytes)
	default:
		key, err = x509.ParsePKCS8PrivateKey(blk.Bytes)
	}
	if err != nil {
		return err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return errors.New("unsupported key type")
	}
	pub, ok := signer.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(cert.PublicKey) {
		return errors.New("key does not belong to the certificate")
	}
	return nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
