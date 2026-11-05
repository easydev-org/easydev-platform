package ci

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// OpsClient is the slice of the execution backend the orchestrator needs.
//
// Every JOB INSTANCE is one run-to-completion job (all of its steps run inside
// it, see supervisor.go); RunJob must NOT block.
type OpsClient interface {
	// RunJob starts a job and returns an identifier.
	RunJob(ctx context.Context, spec JobSpec) (jobID string, err error)
	// WaitJob blocks until the job reaches a terminal state and returns its exit
	// code (0 = success). It MUST return promptly when ctx is canceled, so a
	// cancelled run stops waiting; the caller then calls DeleteJob to kill it.
	WaitJob(ctx context.Context, jobID string) (exitCode int, err error)
	// Logs returns the job's full (or tail) log.
	Logs(ctx context.Context, jobID string) (string, error)
	// DeleteJob removes the job, which is how a cancellation actually STOPS it.
	DeleteJob(ctx context.Context, jobID string) error
}

// LogStreamer is implemented by backends that can STREAM a running job's log.
//
// It is a separate interface (not part of OpsClient) so a backend can be useful
// without it: a run still works on the poll-the-tail path, it just cannot show
// progress live. easyops has WatchJobLogs, so the production path implements it.
type LogStreamer interface {
	// WatchLogs calls emit for each new chunk until the job ends or ctx is
	// canceled. emit must be called from at most one goroutine at a time.
	WatchLogs(ctx context.Context, jobID string, emit func(chunk string)) error
}

// SecretResolver is implemented by backends that can read a tenant secret.
//
// Secrets are resolved as KEY REFERENCES by the executor where possible (a k8s
// secretKeyRef, so the value never enters the Job spec); this interface exists
// for backends without that primitive and for dry runs, and it is used ONLY when
// the backend does not support references.
type SecretResolver interface {
	// Secret returns the secret's key/value pairs.
	Secret(ctx context.Context, owner, name string) (map[string]string, error)
}

// JobSpec is one job instance (all of its steps) to run.
type JobSpec struct {
	// Name is unique within a namespace (the orchestrator makes it so).
	Name      string
	Namespace string
	Owner     string

	Image string
	// Commands is the single script that runs every step. Exactly one entry is
	// used: the supervisor script.
	Commands []string
	// Shell selects the interpreter (bash | sh | pwsh).
	Shell string
	// WorkDir is where the script starts (default /workspace).
	WorkDir string

	Env map[string]string
	// SecretEnv are environment variables sourced from a SECRET REFERENCE rather
	// than a literal, so the value never appears in the Job spec or in a log.
	SecretEnv []SecretEnvVar
	// Secrets are resolved values from a SecretResolver (backends without
	// references). Empty for the k8s path.
	Secrets map[string]string

	CPU            string
	Memory         string
	TimeoutSeconds int

	// Checkout, when non-nil, makes the EXECUTOR materialise a source tree
	// before the script runs. The orchestrator leaves it nil (its supervisor
	// script fetches the tree itself, once per job).
	Checkout *CheckoutSpec
	// Services are companion containers started alongside the job.
	Services map[string]ServiceSpec

	// RunnerLabels selects a runner pool (k8s: node selector / tolerations).
	RunnerLabels []string

	// Workspace is a PVC claim mounted at /workspace (cache reuse).
	Workspace string
}

// SecretEnvVar binds an environment variable to a key of an existing secret.
//
// The reference is rendered as a k8s secretKeyRef, so the VALUE never travels
// through this struct, the Job spec, or the orchestrator's memory.
type SecretEnvVar struct {
	// Env is the variable name exposed to the script.
	Env string
	// Secret is the secret object's name.
	Secret string
	// Key is the key inside the secret.
	Key string
}

// CheckoutSpec tells the executor to materialise a source tree before the
// script runs.
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

// ServiceSpec is a companion container started next to the job.
type ServiceSpec struct {
	Image   string
	Env     map[string]string
	Ports   []int
	Command []string
	Args    []string
}

// Script flattens commands into ONE shell script.
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

// FakeOps is an in-process OpsClient for tests and for `--ci-dry-run`.
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
	// Stream makes WatchLogs available (chunked delivery of LogFor's output).
	Stream bool
	// Canceled records job ids that were deleted before finishing.
	Canceled []string
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
	code, ok := f.exit[jobID]
	f.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("ci: unknown job %q", jobID)
	}
	// Honour cancellation, like the real backend: a canceled wait must return
	// promptly so the orchestrator can delete the job.
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	return code, nil
}

// Logs returns the recorded log.
func (f *FakeOps) Logs(ctx context.Context, jobID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[jobID], nil
}

// DeleteJob records a cancellation and removes the job.
func (f *FakeOps) DeleteJob(ctx context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[jobID]; ok {
		f.Canceled = append(f.Canceled, jobID)
		delete(f.jobs, jobID)
	}
	return nil
}

// WatchLogs streams the recorded log in two chunks (when Stream is set), so the
// streaming path can be tested without a cluster.
func (f *FakeOps) WatchLogs(ctx context.Context, jobID string, emit func(string)) error {
	f.mu.Lock()
	log := f.logs[jobID]
	f.mu.Unlock()
	if !f.Stream {
		return fmt.Errorf("ci: mock: streaming is not enabled (set FakeOps.Stream)")
	}
	if len(log) > 0 {
		mid := len(log) / 2
		emit(log[:mid])
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		emit(log[mid:])
	}
	return nil
}

// Specs returns the recorded specs (copy), for assertions.
func (f *FakeOps) Specs() []JobSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]JobSpec(nil), f.Calls...)
}
