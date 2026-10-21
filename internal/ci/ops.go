package ci

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// OpsClient is the slice of easyops the orchestrator needs. It is an interface
// (not the generated Connect client) so the orchestrator is testable without a
// cluster, and so an in-process fake can stand in for easyops.
//
// In production this is backed by easyops.v1.OpsService over Connect RPC:
//   - CreateJob  -> easyops.v1.OpsService/CreateJob
//   - JobLogs    -> .../JobLogs
//   - DeleteJob  -> .../DeleteJob
//
// easyops runs each Job as a batch/v1.Job (a container to completion), which is
// exactly the shape of a CI job.
type OpsClient interface {
	// RunJob starts a job and returns an identifier. It must NOT block until
	// completion: the orchestrator polls WaitJob / streams logs.
	RunJob(ctx context.Context, spec JobSpec) (jobID string, err error)
	// WaitJob blocks until the job reaches a terminal state and returns its
	// exit code (0 = success).
	WaitJob(ctx context.Context, jobID string) (exitCode int, err error)
	// Logs returns the job's logs (tail).
	Logs(ctx context.Context, jobID string) (string, error)
	// DeleteJob removes the job (best-effort cleanup).
	DeleteJob(ctx context.Context, jobID string) error
}

// JobSpec is one job to run on easyops, already resolved by the orchestrator.
type JobSpec struct {
	// Name is unique within a namespace (the orchestrator makes it so).
	Name      string
	Namespace string
	Owner     string
	Image     string
	Commands  []string
	Env       map[string]string
	CPU       string
	Memory    string
	// TimeoutSeconds bounds the job (0 = no deadline).
	TimeoutSeconds int
	// Checkout, when non-empty, is a source archive URL the job fetches first
	// (easyvcs archive of the run's head snapshot).
	Checkout string
}

// FakeOps is an in-process OpsClient for tests and for `--ci-dry-run`. It
// "runs" a job by recording it and returning a configurable result.
type FakeOps struct {
	mu   sync.Mutex
	jobs map[string]JobSpec
	exit map[string]int
	logs map[string]string
	seq  int
	// FailJob, when non-nil, decides a job's exit code by job name.
	FailJob func(name string) int
}

// NewFakeOps returns an empty fake.
func NewFakeOps() *FakeOps {
	return &FakeOps{jobs: map[string]JobSpec{}, exit: map[string]int{}, logs: map[string]string{}}
}

func (f *FakeOps) RunJob(ctx context.Context, spec JobSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("fake-job-%d-%s", f.seq, spec.Name)
	f.jobs[id] = spec
	code := 0
	if f.FailJob != nil {
		code = f.FailJob(spec.Name)
	}
	f.exit[id] = code
	f.logs[id] = fmt.Sprintf("[fake] ran %s with %d command(s); exit=%d\n", spec.Name, len(spec.Commands), code)
	return id, nil
}

func (f *FakeOps) WaitJob(ctx context.Context, jobID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.exit[jobID]
	if !ok {
		return 0, fmt.Errorf("ci: unknown job %q", jobID)
	}
	return code, nil
}

func (f *FakeOps) Logs(ctx context.Context, jobID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs[jobID], nil
}

func (f *FakeOps) DeleteJob(ctx context.Context, jobID string) error { return nil }

// Script flattens a job's commands into ONE shell script. easyops takes
// `command` + `args`; we run `/bin/sh -c "<script>"` so multi-line scripts and
// `set -e` behave predictably.
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
