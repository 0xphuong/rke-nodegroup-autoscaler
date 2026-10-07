package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/provider"
)

type pki struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) pki {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return pki{cert, key}
}

func (ca pki) issue(t *testing.T, cn string, dns ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: dns,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func (ca pki) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// TestGRPCRequiresClientCertificate runs the real mTLS setup: only a client certificate from the release CA
// may talk to the provider (cluster-autoscaler); anything else is refused at the TLS handshake.
func TestGRPCRequiresClientCertificate(t *testing.T) {
	ca := newCA(t)
	dir := t.TempDir()
	srvCert, srvKey := ca.issue(t, "provider", "provider.test")
	for name, data := range map[string][]byte{"tls.crt": srvCert, "tls.key": srvKey, "ca.crt": ca.pem()} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tlsCfg, err := mtlsConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	protos.RegisterCloudProviderServer(gs, &provider.Provider{Config: &config.Config{
		NodeGroups: []config.NodeGroup{{Name: "app", MinSize: 0, MaxSize: 3}},
	}})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.pem())
	call := func(clientCerts []tls.Certificate) error {
		creds := credentials.NewTLS(&tls.Config{RootCAs: roots, ServerName: "provider.test", Certificates: clientCerts, MinVersion: tls.VersionTLS12})
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(creds))
		if err != nil {
			return err
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		res, err := protos.NewCloudProviderClient(conn).NodeGroups(ctx, &protos.NodeGroupsRequest{})
		if err == nil && (len(res.NodeGroups) != 1 || res.NodeGroups[0].Id != "app") {
			t.Fatalf("unexpected node groups %v", res.NodeGroups)
		}
		return err
	}

	cCert, cKey := ca.issue(t, "cluster-autoscaler")
	good, _ := tls.X509KeyPair(cCert, cKey)
	if err := call([]tls.Certificate{good}); err != nil {
		t.Fatalf("client with a certificate from the release CA must be accepted: %v", err)
	}
	if err := call(nil); err == nil {
		t.Fatal("client without a certificate must be refused")
	}
	other := newCA(t)
	oCert, oKey := other.issue(t, "intruder")
	bad, _ := tls.X509KeyPair(oCert, oKey)
	if err := call([]tls.Certificate{bad}); err == nil {
		t.Fatal("client certificate from another CA must be refused")
	}
}
