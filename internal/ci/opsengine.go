package ci

import (
	"context"
	"fmt"
	"time"

	"github.com/easydev-org/easydev-platform/internal/ops"
)

// EngineOps adapts the platform's tenant-safe ops.Engine to ci.OpsClient, so the
// CI orchestrator runs REAL Kubernetes Jobs instead of the fake.
//
// Every job is submitted with the RUN's namespace as the tenant, so the registry
// row (and the k8s annotations) carry the tenant — and a run for tenant A can
// never touch tenant B's job.
type EngineOps struct {
	Engine *ops.Engine
	// Tenant is the namespace every job is submitted under (the run's tenant).
	Tenant string
	// Timeout bounds WaitJob (0 = engine default).
	Timeout time.Duration
	// ids maps the opaque job id returned to ci back to the ops resource name.
	ids map[string]string
}

// NewEngineOps wraps an engine for one tenant.
func NewEngineOps(e *ops.Engine, tenant string) *EngineOps {
	return &EngineOps{Engine: e, Tenant: tenant, Timeout: 30 * time.Minute, ids: map[string]string{}}
}

// EngineOpsFactory is an OpsClient that BINDS ITSELF TO THE RUN'S TENANT. The
// CI orchestrator calls ForTenant once per run; the returned client submits
// every job under that tenant, so two runs for different tenants can never see
// each other's k8s objects.
//
// It implements OpsClient directly (for single-tenant use) AND exposes
// ForTenant for the multi-tenant path.
type EngineOpsFactory struct {
	Engine *ops.Engine
}

// NewEngineOpsFactory wraps an engine for per-run tenant binding.
func NewEngineOpsFactory(e *ops.Engine) *EngineOpsFactory { return &EngineOpsFactory{Engine: e} }

// ForTenant returns a client bound to one tenant.
func (f *EngineOpsFactory) ForTenant(tenant string) OpsClient {
	return NewEngineOps(f.Engine, tenant)
}

// RunJob (single-tenant fallback): uses the empty tenant, which the engine
// rejects — so callers MUST go through ForTenant in multi-tenant mode.
func (f *EngineOpsFactory) RunJob(ctx context.Context, spec JobSpec) (string, error) {
	return "", fmt.Errorf("ci: EngineOpsFactory needs a tenant; call ForTenant")
}
func (f *EngineOpsFactory) WaitJob(ctx context.Context, jobID string) (int, error) {
	return 0, fmt.Errorf("ci: EngineOpsFactory needs a tenant; call ForTenant")
}
func (f *EngineOpsFactory) Logs(ctx context.Context, jobID string) (string, error) {
	return "", fmt.Errorf("ci: EngineOpsFactory needs a tenant; call ForTenant")
}
func (f *EngineOpsFactory) DeleteJob(ctx context.Context, jobID string) error {
	return fmt.Errorf("ci: EngineOpsFactory needs a tenant; call ForTenant")
}

// RunJob submits a tenant-scoped k8s Job and returns its id.
func (o *EngineOps) RunJob(ctx context.Context, spec JobSpec) (string, error) {
	if o.Engine == nil {
		return "", fmt.Errorf("ci: ops engine is not configured")
	}
	rec, err := o.Engine.SubmitJob(ctx, o.Tenant, ops.JobSpec{
		Name:           spec.Name,
		Image:          spec.Image,
		Commands:       spec.Commands,
		Env:            spec.Env,
		CPU:            spec.CPU,
		Memory:         spec.Memory,
		TimeoutSeconds: spec.TimeoutSeconds,
		Owner:          spec.Owner,
	})
	if err != nil {
		return "", err
	}
	return rec.Name, nil // the tenant-facing name is our id
}

// WaitJob blocks until the tenant's job finishes and maps the state to an exit
// code (0 = succeeded).
func (o *EngineOps) WaitJob(ctx context.Context, jobID string) (int, error) {
	j, err := o.Engine.WaitJob(ctx, o.Tenant, jobID, o.Timeout)
	if err != nil {
		return 0, err
	}
	if j.State == "succeeded" {
		return 0, nil
	}
	code := int(j.ExitCode)
	if code == 0 {
		code = 1 // failed without a reported code
	}
	return code, nil
}

// Logs returns the job log tail.
func (o *EngineOps) Logs(ctx context.Context, jobID string) (string, error) {
	return o.Engine.JobLogs(ctx, o.Tenant, jobID, 200)
}

// DeleteJob removes the job.
func (o *EngineOps) DeleteJob(ctx context.Context, jobID string) error {
	return o.Engine.DeleteJob(ctx, o.Tenant, jobID)
}
