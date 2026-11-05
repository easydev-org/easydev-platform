package ci

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// OpsClient is the slice of the execution backend the orchestrator needs. It is
// an interface (not the generated Connect client) so the orchestrator is
// testable without a cluster, and so several backends can stand in:
//
//   - EngineOps   — in-process easyops library packages (k8s Jobs)
//   - ConnectOps  — easyops.v1.OpsService over Connect (the production path)
//   - RunnerOps   — a self-hosted runner pool (cluster-external execution)
//   - FakeOps     — in-process, for tests and dry runs
//
// Every step is submitted as one run-to-completion job; the orchestrator never
// blocks on RunJob (it polls WaitJob), so a long job cannot pin a goroutine to a
// request context.
type OpsClient interface {
	// RunJob starts a job and returns an identifier. It must NOT block until
	// completion.
	RunJob(ctx context.Context, spec JobSpec) (jobID string, err error)
	// WaitJob blocks until the job reaches a terminal state and returns its
	// exit code (0 = success).
	WaitJob(ctx context.Context, jobID string) (exitCode int, err error)
	// Logs returns the job's log tail.
	Logs(ctx context.Context, jobID string) (string, error)
	// DeleteJob removes the job (best-effort cleanup).
	DeleteJob(ctx context.Context, jobID string) error
}

// CheckoutSpec tells the executor to materialise a source tree before the step
// script runs. The platform NEVER clones git: it fetches easyvcs' subtree
// archive, so the checkout is the same pinned snapshot the workflow came from.
type CheckoutSpec struct {
	// Ref is a snapshot sha, branch, tag or revision id.
	Ref string
	// Path is a sub-scope ("" = the whole namespace).
	Path string
	// Dest is where the tree is unpacked (default ".").
	Dest string
	// URL is the absolute archive URL; empty means "derive from the executor's
	// easyvcs base URL".
	URL string
}

// ServiceSpec is a companion container started next to the step.
type ServiceSpec struct {
	Image   string
	Env     map[string]string
	Ports   []int
	Command []string
	Args    []string
}

// JobSpec is one job to run on the backend, already resolved by the
// orchestrator. It is derived from ONE workflow step.
type JobSpec struct {
	// Name is unique within a namespace (the orchestrator makes it so).
	Name      string
	Namespace string
	Owner     string

	Image string
	// Commands is the shell script. Exactly one entry is used by the k8s
	// backend (it becomes `/bin/sh -c "<script>"`); the runner backend may run
	// them one by one.
	Commands []string
	// Shell selects the interpreter: bash | sh | pwsh (default bash).
	Shell string
	// WorkDir is where the script runs (default /workspace).
	WorkDir string

	Env map[string]string

	CPU    string
	Memory string
	// TimeoutSeconds bounds the job (0 = no deadline).
	TimeoutSeconds int

	// Checkout, when non-nil, materialises a source tree before the script.
	Checkout *CheckoutSpec
	// Services, when non-empty, are started alongside the job.
	Services map[string]ServiceSpec

	// RunnerLabels selects a runner pool (k8s: node selector / tolerations).
	RunnerLabels []string

	// Workspace is a PVC claim mounted at /workspace (cache reuse). Empty = an
	// ephemeral emptyDir.
	Workspace string
}

// Script flattens a job's commands into ONE shell script (used by the k8s
// backend, where a step is one container). `set -e` makes a failing command stop
// the script, which is what `run:` blocks rely on.
func Script(cmds []string) (command []string, args []string) {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, c := range cmds {
		b.WriteString(c)
		b.WriteString("\n")
	}
	return []string{"/bin/sh", "-c"}, []string{b.String()}
}

// Drain reads the remaining bytes of r into a string (bounded by max).
func Drain(r io.Reader, max int) string {
	b, _ := io.ReadAll(io.LimitReader(r, int64(max)))
	return string(b)
}

// FakeOps is an in-process OpsClient for tests and for `--ci-dry-run`. It
// "runs" a job by recording it and returning a configurable result.
type FakeOps struct {
	mu   sync.Mutex
	jobs map[string]JobSpec
	exit map[string]int
	logs map[string]string
	seq  int
	// FailJob, when non-nil, decides a job's exit code from its spec.
	FailJob func(spec JobSpec) int
	// LogFor, when non-nil, supplies the captured log.
	LogFor func(spec JobSpec) string
	// Calls records the specs in submission order (assertions in tests).
	Calls []JobSpec
}

// NewFakeOps returns an empty fake.
func NewFakeOps() *FakeOps {
	return &FakeOps{jobs: map[string]JobSpec{}, exit: map[string]int{}, logs: map[string]string{}}
}

// RunJob records the job and returns a synthetic id.
func (f *FakeOps) RunJob(ctx context.Context, spec JobSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("fake-job-%d-%s", f.seq, spec.Name)
	f.jobs[id] = spec
	f.Calls = append(f.Calls, spec)
	code := 0
	if f.FailJob != nil {
		code = f.FailJob(spec)
	}
	f.exit[id] = code
	log := fmt.Sprintf("[fake] ran %s with %d command(s); exit=%d\n", spec.Name, len(spec.Commands), code)
	if f.LogFor != nil {
		log = f.LogFor(spec)
	}
	f.logs[id] = log
	return id, nil
}

// WaitJob returns the recorded exit code.
func (f *FakeOps) WaitJob(ctx context.Context, jobID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.exit[jobID]
	if !ok {
		return 0, fmt.Errorf("ci: unknown job %q", jobID)
	}
	return code, nil
}

// Logs returns the recorded log.
func (f *FakeOps) Logs(ctx context.Context, jobID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[jobID], nil
}

// DeleteJob is a no-op.
func (f *FakeOps) DeleteJob(ctx context.Context, jobID string) error { return nil }

// Specs returns the recorded specs (copy), for assertions.
func (f *FakeOps) Specs() []JobSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]JobSpec(nil), f.Calls...)
}
