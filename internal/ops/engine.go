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

// ServiceSpec is a companion container started alongside a CI job.
//
// It exists so a job can reach a database/cache by hostname without the workflow
// having to create Deployments itself. The companion is created as a
// single-replica Deployment with a Service, both owned by the tenant.
type ServiceSpec struct {
	Image   string
	Env     map[string]string
	Ports   []int
	Command []string
	Args    []string
}

// JobSpec is a tenant-facing CI job request.
type JobSpec struct {
	Name           string // tenant's own name (any string)
	Image          string
	Commands       []string
	Env            map[string]string
	Shell          string
	WorkDir        string
	CPU            string
	Memory         string
	TimeoutSeconds int
	Owner          string
	// Services are created before the job and removed with it.
	Services map[string]ServiceSpec
	// Workspace is a PVC claim mounted at /workspace ("" = ephemeral emptyDir).
	Workspace string
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
	cmd, args := shellCommandFor(spec)
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
		// CI jobs run UNTRUSTED code: never hand them the namespace's
		// ServiceAccount token, and drop every capability.
		Untrusted: true,
		Workspace: spec.Workspace,
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

// shellCommandFor turns a step into (command, args). A CI step is exactly one
// script, so `/bin/sh -c '<script>'` is the command; `Shell` selects the
// interpreter when the workflow asked for a non-POSIX one.
func shellCommandFor(spec JobSpec) ([]string, []string) {
	script := strings.Join(spec.Commands, "\n")
	switch strings.ToLower(spec.Shell) {
	case "pwsh", "powershell":
		return []string{"pwsh", "-NoProfile", "-Command"}, []string{script}
	case "sh":
		return []string{"/bin/sh", "-c"}, []string{script}
	default: // bash
		return []string{"/bin/bash", "-eo", "pipefail", "-c"}, []string{script}
	}
}

// shellCommand is kept for the ops HTTP surface (no shell selection there).
func shellCommand(cmds []string) ([]string, []string) {
	return shellCommandFor(JobSpec{Commands: cmds})
}

// GetJob returns the tenant's job view; another tenant's job is ErrNotFound.
func (e *Engine) GetJob(ctx context.Context, tenant, name string) (*Resource, *jobmgr.Job, error) {
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return nil, nil, err
	}
	if e.Jobs == nil {
		return rec, nil, nil // registry-only view (no cluster): still tenant-safe
	}
	j, err := e.Jobs.Get(ctx, rec.K8sName)
	if err != nil {
		return rec, nil, fmt.Errorf("ops: get job: %w", err)
	}
	return rec, j, nil
}

// ListJobs returns the tenant's jobs (registry rows joined with live state).
func (e *Engine) ListJobs(ctx context.Context, tenant string) ([]*Resource, error) {
	return e.Reg.List(ctx, tenant, KindJob)
}

// WaitJob blocks until the tenant's job finishes (or the timeout elapses).
func (e *Engine) WaitJob(ctx context.Context, tenant, name string, timeout time.Duration) (*jobmgr.Job, error) {
	if e.Jobs == nil {
		return nil, errors.New("ops: kubernetes backend is not configured")
	}
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return nil, err
	}
	j, err := e.Jobs.Wait(ctx, rec.K8sName, timeout)
	if err != nil {
		return nil, err
	}
	rec.State = j.State
	_ = e.Reg.Update(ctx, rec)
	return j, nil
}

// JobLogs returns the log tail of the tenant's job.
func (e *Engine) JobLogs(ctx context.Context, tenant, name string, tail int64) (string, error) {
	if e.Jobs == nil {
		return "", errors.New("ops: kubernetes backend is not configured")
	}
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return "", err
	}
	return e.Jobs.Logs(ctx, rec.K8sName, "", tail)
}

// DeleteJob removes the tenant's job.
//
// The registry lookup happens BEFORE the backend check, so another tenant's job
// is ErrNotFound (404) even when the cluster is unavailable: existence must
// never leak, and the answer must not depend on the backend's health.
func (e *Engine) DeleteJob(ctx context.Context, tenant, name string) error {
	rec, err := e.Reg.Get(ctx, tenant, KindJob, name)
	if err != nil {
		return err
	}
	if e.Jobs != nil {
		if err := e.Jobs.Delete(ctx, rec.K8sName, "background"); err != nil {
			return err
		}
	}
	return e.Reg.Delete(ctx, tenant, KindJob, name)
}
