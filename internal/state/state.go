// Package state is the provider's record of every VM it created, persisted in one ConfigMap.
//
// It is the source of truth for node group membership and target size: the VNG Cloud API cannot list servers,
// so a VM the provider did not record is never touched. Every mutation is written through before returning.
// The provider runs as a single replica (Recreate strategy), so there is one writer.
package state

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const dataKey = "instances.json"

type Phase string

const (
	// Creating: VM requested (ID may still be empty), not yet a Ready node.
	Creating Phase = "Creating"
	// Running: VM active and registered as a Ready node.
	Running Phase = "Running"
	// Deleting: VM deletion requested; the record goes away once the VM is gone and the Node object deleted.
	Deleting Phase = "Deleting"
	// Failed: VM errored or never registered within maxProvisionTime; reported to cluster-autoscaler,
	// which deletes it.
	Failed Phase = "Failed"
)

type Record struct {
	Name       string    `json:"name"`
	Group      string    `json:"group"`
	ID         string    `json:"id,omitempty"`
	ProviderID string    `json:"providerID,omitempty"`
	Phase      Phase     `json:"phase"`
	CreatedAt  time.Time `json:"createdAt"`
	// TokenHash is the sha256 of the VM's bootstrap token; cleared once the node registers.
	TokenHash   string     `json:"tokenHash,omitempty"`
	TokenExpiry time.Time  `json:"tokenExpiry,omitempty"`
	DeletingAt  *time.Time `json:"deletingAt,omitempty"`
	// NodeMissingSince: a Running instance's Node object has been absent since then (VM still there)
	NodeMissingSince *time.Time `json:"nodeMissingSince,omitempty"`
	Message          string     `json:"message,omitempty"`
}

type Store struct {
	kube      kubernetes.Interface
	namespace string
	name      string

	mu      sync.Mutex
	records map[string]Record
	rv      string
}

func New(kube kubernetes.Interface, namespace, name string) *Store {
	return &Store{kube: kube, namespace: namespace, name: name, records: map[string]Record{}}
}

// Load reads the ConfigMap, creating it empty if it does not exist.
func (s *Store) Load(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cm, err := s.kube.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm, err = s.kube.CoreV1().ConfigMaps(s.namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace},
			Data:       map[string]string{dataKey: "[]"},
		}, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("load state configmap %s/%s: %w", s.namespace, s.name, err)
	}
	var list []Record
	if raw := cm.Data[dataKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return fmt.Errorf("decode state configmap %s/%s: %w", s.namespace, s.name, err)
		}
	}
	s.records = map[string]Record{}
	for _, r := range list {
		s.records[r.Name] = r
	}
	s.rv = cm.ResourceVersion
	return nil
}

// persist writes the current records; callers hold s.mu.
func (s *Store) persist(ctx context.Context) error {
	list := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.namespace, ResourceVersion: s.rv},
		Data:       map[string]string{dataKey: string(raw)},
	}
	out, err := s.kube.CoreV1().ConfigMaps(s.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	s.rv = out.ResourceVersion
	return nil
}

// Put inserts or replaces a record and persists. On a persist error the in-memory change is rolled back.
func (s *Store) Put(ctx context.Context, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.records[r.Name]
	s.records[r.Name] = r
	if err := s.persist(ctx); err != nil {
		if had {
			s.records[r.Name] = prev
		} else {
			delete(s.records, r.Name)
		}
		return err
	}
	return nil
}

// Update applies fn to the named record and persists. fn returning false means "no change".
func (s *Store) Update(ctx context.Context, name string, fn func(*Record) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	if !ok {
		return fmt.Errorf("no instance record %q", name)
	}
	prev := r
	if !fn(&r) {
		return nil
	}
	s.records[name] = r
	if err := s.persist(ctx); err != nil {
		s.records[name] = prev
		return err
	}
	return nil
}

func (s *Store) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.records[name]
	if !ok {
		return nil
	}
	delete(s.records, name)
	if err := s.persist(ctx); err != nil {
		s.records[name] = prev
		return err
	}
	return nil
}

func (s *Store) Get(name string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	return r, ok
}

func (s *Store) ByProviderID(pid string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.records {
		if pid != "" && r.ProviderID == pid {
			return r, true
		}
	}
	return Record{}, false
}

// List returns the records of a group ("" for all), oldest first.
func (s *Store) List(group string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Record{}
	for _, r := range s.records {
		if group == "" || r.Group == group {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}
