package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

func newServer(t *testing.T, now time.Time) (*Server, string) {
	t.Helper()
	tmpl, err := workerplane.Load("../workerplane/testdata/worker-template.json")
	if err != nil {
		t.Fatal(err)
	}
	store := state.New(fake.NewSimpleClientset(), "ns", "state")
	if err := store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	token, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), state.Record{
		Name: "dev-app-n1", Group: "app", ID: "vm-1", ProviderID: "vngcloud://vm-1", Phase: state.Creating,
		CreatedAt: now, TokenHash: hash, TokenExpiry: now.Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	bundle := Bundle{}
	for _, f := range CertFiles {
		bundle[f] = []byte("content of " + f)
	}
	return &Server{
		Config:   &config.Config{NodeGroups: []config.NodeGroup{{Name: "app", Labels: map[string]string{"debug": "true"}, Taints: []string{"debug=true:NoSchedule"}}}},
		Store:    store,
		Template: tmpl,
		Bundle:   bundle,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return now },
	}, token
}

func post(s *Server, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/bootstrap", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestBootstrapServesJoinScript(t *testing.T) {
	now := time.Now()
	s, token := newServer(t, now)
	rec := post(s, token, `{"name":"dev-app-n1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	script := rec.Body.String()
	for _, want := range []string{
		"base64 -d >/etc/kubernetes/ssl/kube-node-key.pem",
		"--provider-id=vngcloud://vm-1",
		"--hostname-override=dev-app-n1",
		"--node-labels=debug=true,rke-autoscaler.io/nodegroup=app",
		"--register-with-taints=debug=true:NoSchedule",
		"chmod 600 /etc/kubernetes/ssl/*-key.pem",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("join script lacks %q", want)
		}
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("join script must not be cacheable")
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		if out, err := exec.Command(bash, "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("join script is not valid bash: %v\n%s", err, out)
		}
	}
}

func TestBootstrapDenials(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		token func(string) string
		body  string
		at    time.Time
	}{
		"no token":      {func(string) string { return "" }, `{"name":"dev-app-n1"}`, now},
		"wrong token":   {func(string) string { return "nope" }, `{"name":"dev-app-n1"}`, now},
		"other node":    {func(t string) string { return t }, `{"name":"dev-app-n2"}`, now},
		"expired":       {func(t string) string { return t }, `{"name":"dev-app-n1"}`, now.Add(11 * time.Minute)},
		"malformed":     {func(t string) string { return t }, `not json`, now},
		"empty name":    {func(t string) string { return t }, `{"name":""}`, now},
	} {
		t.Run(name, func(t *testing.T) {
			s, token := newServer(t, now)
			s.Now = func() time.Time { return tc.at }
			rec := post(s, tc.token(token), tc.body)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "BEGIN") || strings.Contains(rec.Body.String(), "content of") {
				t.Fatal("denial leaked certificate material")
			}
		})
	}
}

func TestBootstrapRefusesRegisteredNodes(t *testing.T) {
	now := time.Now()
	s, token := newServer(t, now)
	_ = s.Store.Update(context.Background(), "dev-app-n1", func(r *state.Record) bool { r.Phase = state.Running; return true })
	if rec := post(s, token, `{"name":"dev-app-n1"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("a registered node's token must not work any more, got %d", rec.Code)
	}
}

func TestUserDataIsValidBash(t *testing.T) {
	ud, err := UserData(UserDataParams{
		NodeName: "dev-app-n1", Token: "tok", Endpoints: []string{"https://10.0.0.11:31443", "https://10.0.0.12:31443"},
		CACert: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----", PreJoin: "apt-get install -y docker.io",
		Deadline: 15 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#!/bin/bash", "deadline=$(( $(date +%s) + 900 ))", "'Authorization: Bearer tok'", "https://10.0.0.12:31443"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data lacks %q:\n%s", want, ud)
		}
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		if out, err := exec.Command(bash, "-n", "-c", ud).CombinedOutput(); err != nil {
			t.Errorf("user data is not valid bash: %v\n%s", err, out)
		}
	}
}
