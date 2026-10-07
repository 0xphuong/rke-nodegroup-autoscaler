// Package bootstrap gets a freshly created VM into the cluster.
//
// The VM's user_data carries no credential except a random token that is valid for one VM, until that VM
// registers or the token expires. With it the VM fetches its join script over TLS from the provider's bootstrap
// server; the script carries the shared RKE node certificates and the docker commands of the worker plane.
// Certificates never appear in user_data, which the cloud console and the VM metadata API both expose.
package bootstrap

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

// CertFiles are the files RKE puts in /etc/kubernetes/ssl on every worker. kube-node and kube-proxy client
// certificates are shared by all nodes of an RKE1 cluster.
var CertFiles = []string{
	"kube-ca.pem",
	"kube-node.pem", "kube-node-key.pem",
	"kube-proxy.pem", "kube-proxy-key.pem",
	"kubecfg-kube-node.yaml", "kubecfg-kube-proxy.yaml",
}

// NewToken returns a random token and its sha256 (hex), which is all that gets stored.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// TokenMatches compares in constant time.
func TokenMatches(token, hash string) bool {
	return hash != "" && subtle.ConstantTimeCompare([]byte(HashToken(token)), []byte(hash)) == 1
}

// Bundle is the node certificate set, read from a mounted Secret.
type Bundle map[string][]byte

func LoadBundle(dir string) (Bundle, error) {
	b := Bundle{}
	for _, f := range CertFiles {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, fmt.Errorf("node certificate bundle: %w", err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, fmt.Errorf("node certificate bundle: %s is empty", f)
		}
		b[f] = data
	}
	return b, nil
}

type UserDataParams struct {
	NodeName  string
	Token     string
	Endpoints []string
	CACert    string // PEM of the CA that signed the bootstrap server certificate
	PreJoin   string
	Deadline  time.Duration
}

var userDataTmpl = template.Must(template.New("userdata").Funcs(template.FuncMap{"q": workerplane.Quote}).Parse(
	`#!/bin/bash
# rke-nodegroup-autoscaler bootstrap for {{.NodeName}}
set -uo pipefail
umask 077
exec >>/var/log/rke-nodegroup-bootstrap.log 2>&1
echo "$(date -Is) bootstrap {{.NodeName}} starting"
hostnamectl set-hostname {{q .NodeName}} || true
{{- if .PreJoin}}
if ! ( set -e
{{.PreJoin}}
); then echo "$(date -Is) pre-join script failed"; exit 1; fi
{{- end}}
ca=$(mktemp)
cat >"$ca" <<'RKE_NG_CA'
{{.CACert}}
RKE_NG_CA
deadline=$(( $(date +%s) + {{.DeadlineSeconds}} ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  for ep in{{range .Endpoints}} {{q .}}{{end}}; do
    if curl -fsS --max-time 30 --cacert "$ca" -H {{q .AuthHeader}} -H 'Content-Type: application/json' \
         -d {{q .Body}} "$ep/v1/bootstrap" -o /root/rke-join.sh; then
      if bash /root/rke-join.sh; then
        rm -f /root/rke-join.sh "$ca"
        echo "$(date -Is) bootstrap {{.NodeName}} done"
        exit 0
      fi
      echo "$(date -Is) join script failed, retrying"
    fi
  done
  sleep 15
done
rm -f /root/rke-join.sh "$ca"
echo "$(date -Is) bootstrap {{.NodeName}} gave up"
exit 1
`))

// UserData renders the cloud-init user_data (a shell script) of one VM.
func UserData(p UserDataParams) (string, error) {
	if p.NodeName == "" || p.Token == "" || len(p.Endpoints) == 0 || p.CACert == "" {
		return "", fmt.Errorf("user data needs node name, token, endpoints and CA certificate")
	}
	var buf bytes.Buffer
	err := userDataTmpl.Execute(&buf, map[string]any{
		"NodeName":        p.NodeName,
		"Endpoints":       p.Endpoints,
		"CACert":          strings.TrimSpace(p.CACert),
		"PreJoin":         strings.TrimSpace(p.PreJoin),
		"DeadlineSeconds": int(p.Deadline.Seconds()),
		"AuthHeader":      "Authorization: Bearer " + p.Token,
		"Body":            fmt.Sprintf(`{"name":%q}`, p.NodeName),
	})
	return buf.String(), err
}

// JoinScript renders the script the bootstrap server hands a VM: certificates, then the worker plane.
func JoinScript(nodeName string, bundle Bundle, images, commands []string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\nset -euo pipefail\numask 077\n")
	fmt.Fprintf(&b, "echo \"$(date -Is) joining as %s\"\n", nodeName)
	b.WriteString("for _ in $(seq 1 100); do docker info >/dev/null 2>&1 && break; sleep 3; done\n")
	b.WriteString("docker info >/dev/null\n")
	// a retried join starts from scratch
	fmt.Fprintf(&b, "for c in %s; do docker rm -f \"$c\" >/dev/null 2>&1 || true; done\n", strings.Join(workerplane.Containers, " "))
	b.WriteString("install -d -m 0755 /etc/kubernetes/ssl\n")
	for _, f := range CertFiles {
		fmt.Fprintf(&b, "base64 -d >/etc/kubernetes/ssl/%s <<'RKE_NG_EOF'\n%s\nRKE_NG_EOF\n", f,
			wrap(base64.StdEncoding.EncodeToString(bundle[f]), 76))
	}
	b.WriteString("chmod 600 /etc/kubernetes/ssl/*-key.pem\nchmod 644 /etc/kubernetes/ssl/kube-ca.pem\n")
	for _, img := range images {
		q := workerplane.Quote(img)
		fmt.Fprintf(&b, "docker image inspect %s >/dev/null 2>&1 || docker pull %s\n", q, q)
	}
	for _, c := range commands {
		b.WriteString(c + "\n")
	}
	fmt.Fprintf(&b, "echo \"$(date -Is) worker plane of %s started\"\n", nodeName)
	return b.String()
}

func wrap(s string, n int) string {
	var b strings.Builder
	for len(s) > n {
		b.WriteString(s[:n] + "\n")
		s = s[n:]
	}
	b.WriteString(s)
	return b.String()
}
