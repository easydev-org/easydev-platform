// Package ops is the platform's Kubernetes-ops engine. It wraps the easyops
// library packages (github.com/easy-platform/easyops/k8s/...) and enforces
// TENANT isolation at the platform layer.
//
// Why a registry table: the k8s object name must be GLOBALLY unique inside the
// single shared k8s namespace, while a tenant's own name is human-friendly (any
// string, incl. non-ASCII). So the platform MINTS the k8s name and stores the
// mapping — it never derives the name from the tenant name, and never relies on
// easyops' List (which has no tenant filter).
//
//	ops_resources (namespace, kind, name)  ->  k8s_name (unique)
//
// Every read/write goes through this registry, so a tenant can only ever see and
// touch its own rows.
package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a tenant has no such resource (also used to hide
// another tenant's resource: existence is never leaked).
var ErrNotFound = errors.New("ops: not found")

// Kind is the resource kind a tenant owns.
type Kind string

const (
	KindJob     Kind = "job"
	KindService Kind = "service"
	KindSandbox Kind = "sandbox"
	KindPVC     Kind = "pvc"
	KindImage   Kind = "image"
)

// Prefix is the short k8s-name prefix for a kind.
func (k Kind) Prefix() string {
	switch k {
	case KindJob:
		return "job"
	case KindService:
		return "svc"
	case KindSandbox:
		return "sbx"
	case KindPVC:
		return "pvc"
	case KindImage:
		return "img"
	}
	return "res"
}

// Resource is one row of the registry: the authority for "who owns which k8s
// object".
type Resource struct {
	Namespace string `json:"namespace"`
	Kind      Kind   `json:"kind"`
	Name      string `json:"name"`
	K8sName   string `json:"k8s_name"`
	K8sKind   string `json:"k8s_kind"`
	Owner     string `json:"owner"`
	State     string `json:"state"`
	Created   int64  `json:"created"`
	Updated   int64  `json:"updated"`
	Deleted   int64  `json:"deleted,omitempty"`
}

// Registry is the durable (namespace,kind,name) to k8s_name mapping.
type Registry interface {
	Create(ctx context.Context, r *Resource) (*Resource, error)
	Get(ctx context.Context, ns string, k Kind, name string) (*Resource, error)
	List(ctx context.Context, ns string, k Kind) ([]*Resource, error)
	Update(ctx context.Context, r *Resource) error
	Delete(ctx context.Context, ns string, k Kind, name string) error
}

// slugify turns a tenant/name into a DNS-1123-ish fragment. Non-ASCII runs
// collapse; the caller always appends randomness, so uniqueness never depends
// on the slug.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 16 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "t"
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:n*2]
	}
	return hex.EncodeToString(b)
}

// MintK8sName builds prefix-slug-rand. The random suffix is what makes the name
// unique; callers retry on a uniqueness collision.
func MintK8sName(k Kind, tenant, name string) string {
	return fmt.Sprintf("%s-%s-%s", k.Prefix(), slugify(tenant), randHex(3))
}

// MemRegistry is an in-memory Registry for tests and single-process runs.
type MemRegistry struct {
	mu   sync.Mutex
	rows map[string]*Resource
	k8s  map[string]string
}

// NewMemRegistry returns an empty registry.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{rows: map[string]*Resource{}, k8s: map[string]string{}}
}

func rowKey(ns string, k Kind, name string) string {
	return ns + "\x00" + string(k) + "\x00" + name
}

// Create mints a unique k8s name and stores the row.
func (m *MemRegistry) Create(ctx context.Context, r *Resource) (*Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rowKey(r.Namespace, r.Kind, r.Name)
	if old, ok := m.rows[key]; ok && old.Deleted == 0 {
		return nil, fmt.Errorf("ops: %s %q already exists in %q", r.Kind, r.Name, r.Namespace)
	}
	cp := *r
	for i := 0; i < 8; i++ {
		cand := MintK8sName(r.Kind, r.Namespace, r.Name)
		if _, taken := m.k8s[cand]; !taken {
			cp.K8sName = cand
			break
		}
	}
	if cp.K8sName == "" {
		return nil, errors.New("ops: could not mint a unique k8s name")
	}
	now := time.Now().UTC().UnixMilli()
	cp.Created, cp.Updated, cp.Deleted = now, now, 0
	if cp.K8sKind == "" {
		cp.K8sKind = defaultK8sKind(r.Kind)
	}
	m.rows[key] = &cp
	m.k8s[cp.K8sName] = key
	out := cp
	return &out, nil
}

func defaultK8sKind(k Kind) string {
	switch k {
	case KindJob:
		return "Job"
	case KindService, KindSandbox:
		return "Deployment"
	case KindPVC:
		return "PersistentVolumeClaim"
	}
	return ""
}

// Get returns the tenant's row (soft-deleted rows are not found).
func (m *MemRegistry) Get(ctx context.Context, ns string, k Kind, name string) (*Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[rowKey(ns, k, name)]
	if !ok || r.Deleted != 0 {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

// List returns the tenant's live rows of one kind, newest first.
func (m *MemRegistry) List(ctx context.Context, ns string, k Kind) ([]*Resource, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Resource, 0)
	for _, r := range m.rows {
		if r.Namespace == ns && r.Kind == k && r.Deleted == 0 {
			cp := *r
			out = append(out, &cp)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Created > out[j-1].Created; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Update writes state back (never changes the k8s name).
func (m *MemRegistry) Update(ctx context.Context, r *Resource) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rowKey(r.Namespace, r.Kind, r.Name)
	old, ok := m.rows[key]
	if !ok || old.Deleted != 0 {
		return ErrNotFound
	}
	cp := *r
	cp.K8sName = old.K8sName
	cp.Updated = time.Now().UTC().UnixMilli()
	m.rows[key] = &cp
	return nil
}

// Delete soft-deletes the row.
func (m *MemRegistry) Delete(ctx context.Context, ns string, k Kind, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[rowKey(ns, k, name)]
	if !ok || r.Deleted != 0 {
		return ErrNotFound
	}
	r.Deleted = time.Now().UTC().UnixMilli()
	return nil
}
