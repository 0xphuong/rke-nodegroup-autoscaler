package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// CopyBundle copies only the listed files (never e.g. kube-ca-key.pem next to them) with mode 0440.
func TestCopyBundle(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	for _, f := range append([]string{"kube-ca-key.pem", "kube-apiserver-key.pem"}, CertFiles...) {
		if err := os.WriteFile(filepath.Join(from, f), []byte("data of "+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := CopyBundle(from, to); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(to)
	if len(entries) != len(CertFiles) {
		t.Fatalf("copied %d files, want exactly the %d CertFiles", len(entries), len(CertFiles))
	}
	for _, f := range CertFiles {
		st, err := os.Stat(filepath.Join(to, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o440 {
			t.Errorf("%s: mode %o, want 440", f, st.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(to, "kube-ca-key.pem")); err == nil {
		t.Fatal("the CA key must never be copied")
	}
	// a missing file fails the copy (the pod then does not start with a half bundle)
	_ = os.Remove(filepath.Join(from, "kube-proxy.pem"))
	if err := CopyBundle(from, t.TempDir()); err == nil {
		t.Fatal("a missing certificate file must fail")
	}
}
