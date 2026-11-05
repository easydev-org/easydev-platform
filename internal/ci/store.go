package ci

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a pipeline run or check does not exist.
var ErrNotFound = errors.New("ci: not found")

// RunState is the lifecycle of a workflow run.
type RunState string

const (
	RunPending  RunState = "pending"
	RunQueued   RunState = "queued"
	RunRunning  RunState = "running"
	RunSuccess  RunState = "success"
	RunFailed   RunState = "failed"
	RunCanceled RunState = "canceled"
	RunSkipped  RunState = "skipped"
	RunTimedOut RunState = "timed_out"
)

// Terminal reports whether the state is final.
func (s RunState) Terminal() bool {
	switch s {
	case RunSuccess, RunFailed, RunCanceled, RunSkipped, RunTimedOut:
		return true
	}
	return false
}

// StepResult is the outcome of one step inside one job instance.
type StepResult struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name,omitempty"`
	State    RunState          `json:"state"`
	ExitCode int               `json:"exit_code,omitempty"`
	Outputs  map[string]string `json:"outputs,omitempty"`
	// LogSHA points at the archived log blob (easyblobs). Empty = not archived.
	LogSHA string `json:"log_sha,omitempty"`
	Error  string `json:"error,omitempty"`
}

// JobResult is the outcome of one JOB INSTANCE (a job × matrix combination).
type JobResult struct {
	// Key is the instance key, e.g. "test" or "test-go1.27".
	Key      string         `json:"key"`
	JobID    string         `json:"job_id"`
	Matrix   map[string]any `json:"matrix,omitempty"`
	State    RunState       `json:"state"`
	ExitCode int            `json:"exit_code,omitempty"`
	Steps    []StepResult   `json:"steps,omitempty"`
	// Outputs are the job's declared outputs (`jobs.<id>.outputs`), resolved
	// from the step output files. They feed `needs.<id>.outputs.<k>`.
	Outputs map[string]string `json:"outputs,omitempty"`
	// LogsTail is a short tail kept inline for quick diagnosis; the full log is
	// archived and addressed by LogSHA.
	LogsTail string `json:"logs_tail,omitempty"`
	LogSHA   string `json:"log_sha,omitempty"`
	Started  int64  `json:"started,omitempty"`
	Finished int64  `json:"finished,omitempty"`
	Error    string `json:"error,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`
}

// Run is one execution of a workflow for one event.
type Run struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	// Workflow is the workflow's display name; WorkflowPath is its path in the
	// namespace; WorkflowSHA is the snapshot the file was read from.
	Workflow     string  `json:"workflow"`
	WorkflowPath string  `json:"workflow_path,omitempty"`
	WorkflowSHA  string  `json:"workflow_sha,omitempty"`
	Trigger      Trigger `json:"trigger"`
	// Ref is the branch/tag the run applies to; Base/Head are the pinned
	// snapshots so a checkout is reproducible.
	Ref  string `json:"ref"`
	Base string `json:"base"`
	Head string `json:"head"`
	// Tag and ChangedPaths carry the event payload (used for filters + the
	// checkout).
	Tag          string   `json:"tag,omitempty"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	// RR is the Revision Request this run belongs to (0 when none).
	RR      int64       `json:"rr,omitempty"`
	State   RunState    `json:"state"`
	Jobs    []JobResult `json:"jobs"`
	Steps   int         `json:"step_count,omitempty"`
	Created int64       `json:"created"`
	Updated int64       `json:"updated"`
	// Attempt increments on a re-run of the same (workflow, ref, head).
	Attempt int `json:"attempt,omitempty"`
	// Owner is the user who triggered the run.
	Owner string `json:"owner"`
	// Actor is the user whose change caused the event (for RRs: the author).
	Actor string `json:"actor,omitempty"`
	// Group is the resolved concurrency group this run belongs to ("" = none).
	// Persisted so the gate is truthful across replicas.
	Group string `json:"group,omitempty"`
}

// Check is a merge-blocking status shown on a Revision Request. One check per
// (RR, workflow) pair; the orchestrator updates it as jobs finish.
type Check struct {
	RR        int64  `json:"rr"`
	Namespace string `json:"namespace"`
	// Name is the CHECK name (the workflow name by default).
	Name     string   `json:"name"`
	State    RunState `json:"state"`
	Required bool     `json:"required"`
	Details  string   `json:"details,omitempty"`
	RunID    string   `json:"run_id,omitempty"`
	// Job summarizes the failing/succeeding job for the UI ("test (go1.27)").
	Job     string `json:"job,omitempty"`
	Updated int64  `json:"updated"`
}

// Store is the durable state the orchestrator needs. MemStore is the default;
// a Postgres implementation is a drop-in behind this interface.
type Store interface {
	PutRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, namespace string, limit int) ([]*Run, error)
	PutCheck(ctx context.Context, c *Check) error
	ListChecks(ctx context.Context, namespace string, rr int64) ([]*Check, error)
	// CancelRun records a cancellation request for a run id.
	CancelRun(ctx context.Context, id string) error
}

// MemStore is an in-memory Store (default; safe for one process).
type MemStore struct {
	mu       sync.Mutex
	runs     map[string]*Run
	checks   map[string]*Check
	canceled map[string]bool
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{runs: map[string]*Run{}, checks: map[string]*Check{}, canceled: map[string]bool{}}
}

func checkKey(ns string, rr int64, name string) string {
	return ns + "\x00" + itoa(rr) + "\x00" + name
}

func (m *MemStore) PutRun(ctx context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.Updated = time.Now().UTC().UnixMilli()
	cp := *r
	cp.Jobs = append([]JobResult(nil), r.Jobs...)
	m.runs[r.ID] = &cp
	return nil
}

func (m *MemStore) GetRun(ctx context.Context, id string) (*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	cp.Jobs = append([]JobResult(nil), r.Jobs...)
	return &cp, nil
}

func (m *MemStore) ListRuns(ctx context.Context, namespace string, limit int) ([]*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Run, 0)
	for _, r := range m.runs {
		if namespace == "" || r.Namespace == namespace {
			cp := *r
			cp.Jobs = append([]JobResult(nil), r.Jobs...)
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemStore) PutCheck(ctx context.Context, c *Check) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.Updated = time.Now().UTC().UnixMilli()
	cp := *c
	m.checks[checkKey(c.Namespace, c.RR, c.Name)] = &cp
	return nil
}

func (m *MemStore) ListChecks(ctx context.Context, namespace string, rr int64) ([]*Check, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Check, 0)
	for _, c := range m.checks {
		if c.Namespace == namespace && c.RR == rr {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *MemStore) CancelRun(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return ErrNotFound
	}
	m.canceled[id] = true
	r.State = RunCanceled
	return nil
}

// isCanceled reports whether a cancellation was requested for the run.
func (m *MemStore) isCanceled(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canceled[id]
}

// IsCanceled implements Cancelable.
func (m *MemStore) IsCanceled(ctx context.Context, id string) bool { return m.isCanceled(id) }

// LiveRunInGroup implements GroupQuerier.
func (m *MemStore) LiveRunInGroup(ctx context.Context, group, excludeRunID string) (string, error) {
	if group == "" {
		return "", nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.runs {
		if id == excludeRunID || r.Group != group {
			continue
		}
		if !r.State.Terminal() {
			return id, nil
		}
	}
	return "", nil
}

// itoa is a tiny int64 formatter to avoid importing strconv in this file.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
