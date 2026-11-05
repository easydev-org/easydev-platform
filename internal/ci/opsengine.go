package ci

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/easydev-org/easydev-platform/internal/ops"
)

// EngineOps adapts the platform's tenant-safe ops.Engine to ci.OpsClient, so the
// CI orchestrator runs REAL Kubernetes Jobs instead of the fake.
//
// Every job is submitted with the RUN's namespace as the tenant, so the registry
// row (and the k8s annotations) carry the tenant — and a run for tenant A can
// never touch tenant B's job.
//
// The checkout is materialised HERE, not by the executor: the step's script is
// prefixed with a `curl | tar` of the pinned easyvcs snapshot, so the job never
// has to know the VCS or hold a token beyond the archive URL.
type EngineOps struct {
	Engine *ops.Engine
	// Tenant is the namespace every job is submitted under (the run's tenant).
	Tenant string
	// Timeout bounds WaitJob (0 = engine default).
	Timeout time.Duration
	// VCSBase is the easyvcs base URL the checkout prefix downloads from.
	VCSBase string
	// VCSUser / VCSToken authenticate the archive fetch. Empty = anonymous.
	VCSUser  string
	VCSToken string
	// ids maps the opaque job id returned to ci back to the ops resource name.
	ids map[string]string
	mu  sync.Mutex
}

// NewEngineOps wraps an engine for one tenant.
func NewEngineOps(e *ops.Engine, tenant string) *EngineOps {
	return &EngineOps{Engine: e, Tenant: tenant, Timeout: 30 * time.Minute, ids: map[string]string{}}
}

// EngineOpsFactory is an OpsClient that BINDS ITSELF TO THE RUN'S TENANT. The
// CI orchestrator calls ForTenant once per run; the returned client submits
// every job under that tenant, so two runs for different tenants can never see
// each other's k8s objects.
type EngineOpsFactory struct {
	Engine   *ops.Engine
	VCSBase  string
	VCSUser  string
	VCSToken string
}

// NewEngineOpsFactory wraps an engine for per-run tenant binding.
func NewEngineOpsFactory(e *ops.Engine) *EngineOpsFactory {
	return &EngineOpsFactory{Engine: e}
}

// ForTenant returns a client bound to one tenant.
func (f *EngineOpsFactory) ForTenant(tenant string) OpsClient {
	o := NewEngineOps(f.Engine, tenant)
	o.VCSBase, o.VCSUser, o.VCSToken = f.VCSBase, f.VCSUser, f.VCSToken
	return o
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
	script, env := o.materialise(spec)
	rec, err := o.Engine.SubmitJob(ctx, o.Tenant, ops.JobSpec{
		Name:           spec.Name,
		Image:          spec.Image,
		Commands:       []string{script},
		Env:            env,
		Shell:          spec.Shell,
		WorkDir:        spec.WorkDir,
		CPU:            spec.CPU,
		Memory:         spec.Memory,
		TimeoutSeconds: spec.TimeoutSeconds,
		Owner:          spec.Owner,
		Services:       toOpsServices(spec.Services),
		Workspace:      spec.Workspace,
	})
	if err != nil {
		return "", err
	}
	o.mu.Lock()
	o.ids[rec.K8sName] = rec.Name
	o.mu.Unlock()
	return rec.K8sName, nil
}

// materialise turns a step into the shell script the container runs: an
// optional checkout prefix followed by the user's script.
//
// The checkout is deliberately a `curl | tar` of the PINNED snapshot: no git
// client, no credentials beyond the archive URL, and the resulting tree is
// byte-for-byte the snapshot the workflow was read from.
func (o *EngineOps) materialise(spec JobSpec) (string, map[string]string) {
	env := make(map[string]string, len(spec.Env)+8)
	for k, v := range spec.Env {
		env[k] = v
	}
	var b strings.Builder
	b.WriteString("set -e\n")

	b.WriteString("mkdir -p /workspace\ncd /workspace\n")

	if spec.Checkout != nil {
		url := spec.Checkout.URL
		if url == "" {
			url = o.archiveURL(spec)
		}
		dest := spec.Checkout.Dest
		if dest == "" {
			dest = "."
		}
		// Check out BEFORE changing into the working directory: the workdir is
		// usually a subdirectory OF the checkout.
		b.WriteString(checkoutScript(url, dest, o.VCSUser, o.VCSToken))
		env["EASYDEV_CHECKOUT_REF"] = spec.Checkout.Ref
		env["EASYDEV_CHECKOUT_PATH"] = spec.Checkout.Path
	}

	workdir := spec.WorkDir
	if workdir != "" && workdir != "." {
		b.WriteString("mkdir -p ")
		b.WriteString(shellQuote(workdir))
		b.WriteString(" && cd ")
		b.WriteString(shellQuote(workdir))
		b.WriteString("\n")
	}

	// Step output channel: the script writes NAME=VALUE lines to this file and
	// the supervisor prints them behind a marker the orchestrator parses.
	env["EASYDEV_OUTPUT"] = "/workspace/.easydev/output"
	b.WriteString("mkdir -p /workspace/.easydev\n: > /workspace/.easydev/output\n")

	for _, c := range spec.Commands {
		b.WriteString(c)
		b.WriteString("\n")
	}
	// Emit the outputs (only if the step produced any).
	b.WriteString("if [ -s /workspace/.easydev/output ]; then\n")
	b.WriteString("  echo '##[easydev:outputs]'\n")
	b.WriteString("  cat /workspace/.easydev/output\n")
	b.WriteString("fi\n")
	return b.String(), env
}

// checkoutScript renders the `curl | tar` prefix for a pinned snapshot.
func checkoutScript(url, dest, user, token string) string {
	var b strings.Builder
	b.WriteString("echo '::easydev:step checkout'\n")
	curl := "curl -fsSL"
	if user != "" || token != "" {
		// -u is passed through an ENV var, never interpolated into the script,
		// so a token containing shell metacharacters cannot break out.
		curl += ` -u "$EASYDEV_CHECKOUT_AUTH"`
	}
	b.WriteString("mkdir -p ")
	b.WriteString(shellQuote(dest))
	b.WriteString("\n")
	b.WriteString(curl)
	b.WriteString(" ")
	b.WriteString(shellQuote(url))
	b.WriteString(" | tar -xz -C ")
	b.WriteString(shellQuote(dest))
	b.WriteString("\n")
	return b.String()
}

// archiveURL builds the easyvcs subtree archive URL for the checkout.
func (o *EngineOps) archiveURL(spec JobSpec) string {
	base := strings.TrimSuffix(o.VCSBase, "/")
	c := spec.Checkout
	if c == nil {
		return ""
	}
	u := fmt.Sprintf("%s/api/v1/namespaces/%s/archive/%s.tar.gz", base, spec.Namespace, c.Ref)
	if c.Path != "" {
		u += "?path=" + c.Path
	}
	return u
}

// WaitJob blocks until the tenant's job finishes and maps the state to an exit
// code (0 = succeeded).
func (o *EngineOps) WaitJob(ctx context.Context, jobID string) (int, error) {
	name := o.lookup(jobID)
	j, err := o.Engine.WaitJob(ctx, o.Tenant, name, o.Timeout)
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
	return o.Engine.JobLogs(ctx, o.Tenant, o.lookup(jobID), 2000)
}

// DeleteJob removes the job.
func (o *EngineOps) DeleteJob(ctx context.Context, jobID string) error {
	return o.Engine.DeleteJob(ctx, o.Tenant, o.lookup(jobID))
}

func (o *EngineOps) lookup(jobID string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if n, ok := o.ids[jobID]; ok {
		return n
	}
	return jobID
}

func toOpsServices(in map[string]ServiceSpec) map[string]ops.ServiceSpec {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ops.ServiceSpec, len(in))
	for k, v := range in {
		out[k] = ops.ServiceSpec{
			Image: v.Image, Env: v.Env, Ports: v.Ports,
			Command: v.Command, Args: v.Args,
		}
	}
	return out
}

// shellQuote single-quotes a string for POSIX sh, escaping embedded quotes.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
