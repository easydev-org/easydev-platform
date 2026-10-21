package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/easy-platform/easyops/k8s/jobmgr"
)

// JobRunner is the slice of easyops' jobmgr the platform uses. It is an
// interface so tests (and a --dry-run mode) can substitute a fake, and so the
// engine never depends on the concrete k8s clientset.
type JobRunner interface {
	Create(ctx context.Context, s jobmgr.Spec) (*jobmgr.Job, error)
	Get(ctx context.Context, name string) (*jobmgr.Job, error)
	Delete(ctx context.Context, name, propagation string) error
	Logs(ctx context.Context, name, container string, tail int64) (string, error)
	Wait(ctx context.Context, name string, timeout time.Duration) (*jobmgr.Job, error)
}

// Engine is the tenant-safe facade over easyops. Every method takes the tenant
// (namespace) and validates ownership through the Registry before touching k8s.
type Engine struct {
	Reg  Registry
	Jobs JobRunner // nil = ops unavailable (503)
}

// NewEngine wires the engine.
func NewEngine(reg Registry, jobs JobRunner) *Engine {
	return &Engine{Reg: reg, Jobs: jobs}
}

// JobSpec is a tenant-facing CI job request.
type JobSpec struct {
	Name           string // tenant's own name (any string)
	Image          string
	Commands       []string
	Env            map[string]string
	CPU            string
	Memory         string
	TimeoutSeconds int
	Owner          string
}

// SubmitJob creates the k8s Job for a tenant. It:
//  1. mints + records a unique k8s name in the registry (authority),
//  2. creates the k8s Job under that name,
//  3. stores the returned state.
func (e *Engine) SubmitJob(ctx context.Context, tenant string, spec JobSpec) (*Resource, error) {
	if e.Jobs == nil {
		return nil, errors.New("ops: kubernetes backend is not configured")
	}
	if tenant == "" {
		return nil, errors.New("ops: tenant is required")
	}
	rec, err := e.Reg.Create(ctx, &Resource{
		Namespace: tenant, Kind: KindJob, Name: spec.Name, Owner: spec.Owner, State: "pending",
	})
	if err != nil {
		return nil, err
	}
	cmd, args := shellCommand(spec.Commands)
	j, err := e.Jobs.Create(ctx, jobmgr.Spec{
		Name:    rec.K8sName,
		Image:   spec.Image,
		Command: cmd,
		Args:    args,
		Env:     spec.Env,
		CPU:     spec.CPU,
		Memory:  spec.Memory,
		Owner:   spec.Owner,
		// Session carries the TENANT so easyops' own annotations record it too
		// (defence in depth: our registry is the authority, this is a mirror).
		Session:               tenant,
		ActiveDeadlineSeconds: int32(spec.TimeoutSeconds),
	})
	if err != nil {
		// Roll the registry row back: k8s never got the object.
		_ = e.Reg.Delete(ctx, tenant, KindJob, spec.Name)
		return nil, fmt.Errorf("ops: create job: %w", err)
	}
	rec.State = j.State
	_ = e.Reg.Update(ctx, rec)
	return rec, nil
}

// GetJob returns the tenant's job view; another tenant's job is ErrNotFound.
func (e *Engine) GetJob(ctx context.Context, tenant, name string) (*Resource, *jobmgr.Job, error) {
	// The REGISTRY is the authority for ownership, so consult it FIRST: another
	// tenant's resource is ErrNotFound even when k8s is down (existence never
	// leaks, and it must not become a 503).
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return nil, nil, err // ErrNotFound for another tenant too
	}
	if e.Jobs == nil {
		return rec, nil, nil // ours, but no live k8s view
	}
	j, err := e.Jobs.Get(ctx, rec.K8sName)
	if err != nil {
		return rec, nil, nil // registry row exists but k8s object is gone
	}
	// Defence in depth: verify the k8s object really belongs to this tenant.
	if !owns(j, tenant) {
		return nil, nil, ErrNotFound
	}
	if j.State != rec.State {
		rec.State = j.State
		_ = e.Reg.Update(ctx, rec)
	}
	return rec, j, nil
}

// ListJobs lists ONLY the tenant's jobs (from the registry, not easyops.List).
func (e *Engine) ListJobs(ctx context.Context, tenant string) ([]*Resource, error) {
	return e.Reg.List(ctx, tenant, KindJob)
}

// DeleteJob removes the tenant's job.
func (e *Engine) DeleteJob(ctx context.Context, tenant, name string) error {
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return err
	}
	if e.Jobs != nil {
		_ = e.Jobs.Delete(ctx, rec.K8sName, "background")
	}
	return e.Reg.Delete(ctx, tenant, KindJob, name)
}

// JobLogs returns the tail of the tenant's job logs.
func (e *Engine) JobLogs(ctx context.Context, tenant, name string, tail int64) (string, error) {
	rec, _, err := e.GetJob(ctx, tenant, name)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", ErrNotFound
	}
	if e.Jobs == nil {
		return "", errors.New("ops: kubernetes backend is not configured")
	}
	return e.Jobs.Logs(ctx, rec.K8sName, "", tail)
}

// WaitJob blocks until the tenant's job finishes and returns its state.
func (e *Engine) WaitJob(ctx context.Context, tenant, name string, timeout time.Duration) (*jobmgr.Job, error) {
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return nil, err
	}
	if e.Jobs == nil {
		return nil, errors.New("ops: kubernetes backend is not configured")
	}
	j, err := e.Jobs.Wait(ctx, rec.K8sName, timeout)
	if err != nil {
		return nil, err
	}
	rec.State = j.State
	_ = e.Reg.Update(ctx, rec)
	return j, nil
}

// owns reports whether the easyops view reports our tenant. easyops stamps
// `Session` from Spec.Session (we pass the tenant) — this is the mirror check.
func owns(j *jobmgr.Job, tenant string) bool {
	if j == nil {
		return false
	}
	if j.Session == "" {
		return true // older objects without the annotation: the registry decided
	}
	return j.Session == tenant
}

// shellCommand flattens a command list into /bin/sh -c "<script>".
func shellCommand(cmds []string) (command, args []string) {
	if len(cmds) == 0 {
		return nil, nil
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, c := range cmds {
		b.WriteString(c)
		b.WriteString("\n")
	}
	return []string{"/bin/sh", "-c"}, []string{b.String()}
}
