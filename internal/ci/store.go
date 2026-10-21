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

// RunState is the lifecycle of a pipeline run.
type RunState string

const (
	RunPending  RunState = "pending"
	RunRunning  RunState = "running"
	RunSuccess  RunState = "success"
	RunFailed   RunState = "failed"
	RunCanceled RunState = "canceled"
)

// JobResult is the outcome of one job in a run.
type JobResult struct {
	JobID    string   `json:"job_id"`
	State    RunState `json:"state"`
	ExitCode int      `json:"exit_code"`
	LogsTail string   `json:"logs_tail,omitempty"`
	Started  int64    `json:"started,omitempty"`
	Finished int64    `json:"finished,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// Run is one execution of a pipeline for one event.
type Run struct {
	ID        string  `json:"id"`
	Namespace string  `json:"namespace"`
	Pipeline  string  `json:"pipeline"`
	Trigger   Trigger `json:"trigger"`
	// Ref is the branch/tag the run applies to; Base is the base snapshot sha
	// (the merge base) so a checkout is reproducible.
	Ref  string `json:"ref"`
	Base string `json:"base"`
	Head string `json:"head"`
	// RR is the Revision Request this run belongs to (0 when none).
	RR      int64       `json:"rr,omitempty"`
	State   RunState    `json:"state"`
	Jobs    []JobResult `json:"jobs"`
	Created int64       `json:"created"`
	Updated int64       `json:"updated"`
	// Owner is the user who triggered the run.
	Owner string `json:"owner"`
}

// Check is a merge-blocking status shown on a Revision Request. One check per
// (RR, pipeline) pair; the orchestrator updates it as jobs finish.
type Check struct {
	RR        int64    `json:"rr"`
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	State     RunState `json:"state"`
	Required  bool     `json:"required"`
	Details   string   `json:"details,omitempty"`
	RunID     string   `json:"run_id,omitempty"`
	Updated   int64    `json:"updated"`
}

// Store is the durable state the orchestrator needs. The in-memory
// implementation is the default; a Postgres implementation is a drop-in.
type Store interface {
	PutRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, namespace string, limit int) ([]*Run, error)
	PutCheck(ctx context.Context, c *Check) error
	ListChecks(ctx context.Context, namespace string, rr int64) ([]*Check, error)
}

// MemStore is an in-memory Store (default; safe for one process).
type MemStore struct {
	mu     sync.Mutex
	runs   map[string]*Run
	checks map[string]*Check
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{runs: map[string]*Run{}, checks: map[string]*Check{}}
}

func checkKey(ns string, rr int64, name string) string {
	return ns + "\x00" + itoa(rr) + "\x00" + name
}

func (m *MemStore) PutRun(ctx context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *r
	r.Updated = time.Now().UTC().UnixMilli()
	cp.Updated = r.Updated
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
	return &cp, nil
}

func (m *MemStore) ListRuns(ctx context.Context, namespace string, limit int) ([]*Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Run, 0)
	for _, r := range m.runs {
		if namespace == "" || r.Namespace == namespace {
			cp := *r
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
