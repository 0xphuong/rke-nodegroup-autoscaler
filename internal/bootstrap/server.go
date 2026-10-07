package bootstrap

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

// Server answers POST /v1/bootstrap from new VMs. It is exposed on a NodePort because the VM is not in the
// cluster yet; the token is the only thing standing between the network and the node certificates, so every
// failure gets the same 403 and the details only go to the log.
type Server struct {
	Config   *config.Config
	Store    *state.Store
	Template *workerplane.Template
	Bundle   Bundle
	Log      *slog.Logger
	Now      func() time.Time
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/v1/bootstrap", s.bootstrap)
	return mux
}

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deny := func(name, why string) {
		s.Log.Warn("bootstrap denied", "node", name, "reason", why, "remote", r.RemoteAddr)
		http.Error(w, "forbidden", http.StatusForbidden)
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		deny("", "no bearer token")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil || body.Name == "" {
		deny("", "bad body")
		return
	}
	rec, ok := s.Store.Get(body.Name)
	switch {
	case !ok:
		deny(body.Name, "unknown instance")
		return
	case rec.Phase != state.Creating:
		deny(body.Name, "instance is "+string(rec.Phase))
		return
	case !TokenMatches(token, rec.TokenHash):
		deny(body.Name, "token mismatch or already used")
		return
	case s.Now().After(rec.TokenExpiry):
		deny(body.Name, "token expired")
		return
	}
	if rec.ProviderID == "" {
		// the VM booted before CreateServer's answer was recorded; the VM retries
		http.Error(w, "not ready, retry", http.StatusServiceUnavailable)
		return
	}
	group, ok := s.Config.Group(rec.Group)
	if !ok {
		deny(body.Name, "node group "+rec.Group+" no longer configured")
		return
	}
	labels := map[string]string{config.GroupLabel: group.Name}
	for k, v := range group.Labels {
		labels[k] = v
	}
	cmds, err := s.Template.Commands(workerplane.Node{
		Name: rec.Name, ProviderID: rec.ProviderID, Labels: labels, Taints: group.Taints,
	})
	if err != nil {
		s.Log.Error("render worker plane", "node", rec.Name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, JoinScript(rec.Name, s.Bundle, s.Template.Images(), cmds))
	s.Log.Info("bootstrap served", "node", rec.Name, "group", rec.Group, "remote", r.RemoteAddr)
}
