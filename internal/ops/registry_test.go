package ops

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRegistryMintsUniqueNames(t *testing.T) {
	r := NewMemRegistry()
	ctx := context.Background()
	a, err := r.Create(ctx, &Resource{Namespace: "team-a", Kind: KindJob, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Create(ctx, &Resource{Namespace: "team-b", Kind: KindJob, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if a.K8sName == b.K8sName {
		t.Fatalf("same-tenant-name collision: %s == %s", a.K8sName, b.K8sName)
	}
	if !strings.HasPrefix(a.K8sName, "job-team-a-") {
		t.Fatalf("k8s name should be readable: %s", a.K8sName)
	}
}

// TestNonASCIITenantNamesWork pins the reason we mint (DB) instead of deriving:
// a Chinese tenant name cannot be a k8s name, but must still work.
func TestNonASCIITenantNamesWork(t *testing.T) {
	r := NewMemRegistry()
	ctx := context.Background()
	a, err := r.Create(ctx, &Resource{Namespace: "我的项目", Kind: KindService, Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if a.K8sName == "" || !strings.HasPrefix(a.K8sName, "svc-") {
		t.Fatalf("bad k8s name: %q", a.K8sName)
	}
	// Two different tenants, both called "web", never collide.
	b, _ := r.Create(ctx, &Resource{Namespace: "另一个项目", Kind: KindService, Name: "web"})
	if a.K8sName == b.K8sName {
		t.Fatal("collision")
	}
}

// TestTenantIsolation is the core security property: one tenant can never see
// or fetch another tenant's row.
func TestTenantIsolation(t *testing.T) {
	r := NewMemRegistry()
	ctx := context.Background()
	if _, err := r.Create(ctx, &Resource{Namespace: "team-a", Kind: KindJob, Name: "secret"}); err != nil {
		t.Fatal(err)
	}
	// team-a sees it.
	if _, err := r.Get(ctx, "team-a", KindJob, "secret"); err != nil {
		t.Fatalf("owner must see its own job: %v", err)
	}
	// team-b must not.
	if _, err := r.Get(ctx, "team-b", KindJob, "secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("team-b must get ErrNotFound, got %v", err)
	}
	// team-b's list is empty.
	list, _ := r.List(ctx, "team-b", KindJob)
	if len(list) != 0 {
		t.Fatalf("team-b must see nothing, got %d", len(list))
	}
	// team-b cannot update or delete team-a's row.
	if err := r.Update(ctx, &Resource{Namespace: "team-b", Kind: KindJob, Name: "secret"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update must fail, got %v", err)
	}
	if err := r.Delete(ctx, "team-b", KindJob, "secret"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete must fail, got %v", err)
	}
	// team-a's row is untouched.
	if _, err := r.Get(ctx, "team-a", KindJob, "secret"); err != nil {
		t.Fatalf("owner's row must survive: %v", err)
	}
}

func TestK8sNameImmutable(t *testing.T) {
	r := NewMemRegistry()
	ctx := context.Background()
	rec, _ := r.Create(ctx, &Resource{Namespace: "n", Kind: KindJob, Name: "x"})
	orig := rec.K8sName
	rec.K8sName = "hijacked"
	rec.State = "running"
	if err := r.Update(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get(ctx, "n", KindJob, "x")
	if got.K8sName != orig {
		t.Fatalf("k8s name must be immutable: %s -> %s", orig, got.K8sName)
	}
	if got.State != "running" {
		t.Fatalf("state must update: %s", got.State)
	}
}

func TestDeleteIsSoft(t *testing.T) {
	r := NewMemRegistry()
	ctx := context.Background()
	_, _ = r.Create(ctx, &Resource{Namespace: "n", Kind: KindJob, Name: "x"})
	if err := r.Delete(ctx, "n", KindJob, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get(ctx, "n", KindJob, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted row must be not-found")
	}
	list, _ := r.List(ctx, "n", KindJob)
	if len(list) != 0 {
		t.Fatal("deleted row must not be listed")
	}
}
