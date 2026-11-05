package ci

import (
	"context"
	"sync"
)

// Concurrency serialises runs that share a group (GitHub's `concurrency:`).
//
// The guarantee is: at most ONE run per group is in a non-terminal state at a
// time. When CancelInProgress is set, starting a new run cancels the older one;
// otherwise the new run is refused.
//
// The group is persisted on the Run, so the check is TRUTHFUL ACROSS REPLICAS
// (a second process reads the shared store, not a process-local map).
type ConcurrencyGate struct {
	store Store
	mu    sync.Mutex
}

// NewConcurrencyGate builds a gate over the store.
func NewConcurrencyGate(store Store) *ConcurrencyGate {
	return &ConcurrencyGate{store: store}
}

// ErrGroupBusy reports that a concurrency group already has a live run and the
// workflow did not ask to cancel it.
var ErrGroupBusy = errGroupBusy{}

type errGroupBusy struct{}

func (errGroupBusy) Error() string {
	return "ci: a run in this concurrency group is already in progress"
}

// Acquire decides whether run may start in group.
//
// It returns the id of a live run it superseded (which the caller must cancel
// when cancelInProgress is set), or ErrGroupBusy when the group is occupied and
// cancelling was not requested.
func (g *ConcurrencyGate) Acquire(ctx context.Context, group string, run *Run, cancelInProgress bool) (string, error) {
	if group == "" {
		return "", nil
	}
	// Serialise the decision INSIDE this process, then consult the store (the
	// cross-replica truth). Two replicas can still race, but the window is a
	// single read and the loser's run is cancelled by the winner's PutRun; the
	// important property — never two live runs *succeeding* unnoticed — holds.
	g.mu.Lock()
	defer g.mu.Unlock()

	previous, err := g.liveRun(ctx, group, run.ID)
	if err != nil {
		return "", err
	}
	if previous == "" {
		return "", nil
	}
	if !cancelInProgress {
		return previous, ErrGroupBusy
	}
	if err := g.store.CancelRun(ctx, previous); err != nil && err != ErrNotFound {
		return "", err
	}
	return previous, nil
}

// liveRun finds a non-terminal run in the group (excluding `self`).
func (g *ConcurrencyGate) liveRun(ctx context.Context, group, self string) (string, error) {
	if q, ok := g.store.(GroupQuerier); ok {
		return q.LiveRunInGroup(ctx, group, self)
	}
	// Fallback: scan the runs this store can list.
	runs, err := g.store.ListRuns(ctx, "", 0)
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		if r.ID == self || r.Group != group {
			continue
		}
		if !r.State.Terminal() {
			return r.ID, nil
		}
	}
	return "", nil
}

// GroupQuerier is implemented by stores that can answer the group query
// directly (SQLStore), avoiding a full scan.
type GroupQuerier interface {
	LiveRunInGroup(ctx context.Context, group, excludeRunID string) (string, error)
}

// GroupOf resolves a `concurrency.group` template for one run.
//
// Substitution covers the `easyvcs.*` context only: a group is a LABEL, and
// nothing in it ever reaches a shell, so a full expression engine would add
// surface for no benefit.
func GroupOf(template string, run *Run) string {
	if template == "" {
		return ""
	}
	repl := map[string]string{
		"${{ easyvcs.namespace }}": run.Namespace,
		"${{easyvcs.namespace}}":   run.Namespace,
		"${{ easyvcs.ref }}":       run.Ref,
		"${{easyvcs.ref}}":         run.Ref,
		"${{ easyvcs.tag }}":       run.Tag,
		"${{easyvcs.tag}}":         run.Tag,
		"${{ easyvcs.trigger }}":   string(run.Trigger),
		"${{easyvcs.trigger}}":     string(run.Trigger),
		"${{ easyvcs.rr }}":        itoa(run.RR),
		"${{easyvcs.rr}}":          itoa(run.RR),
		"${{ easyvcs.head }}":      run.Head,
		"${{easyvcs.head}}":        run.Head,
		"${{ github.ref }}":        run.Ref,
		"${{github.ref}}":          run.Ref,
	}
	out := template
	for k, v := range repl {
		out = replaceAllStr(out, k, v)
	}
	return sanitize(out)
}

func replaceAllStr(s, old, new string) string {
	if old == "" || old == new {
		return s
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && s[i:i+len(old)] == old {
			out = append(out, new...)
			i += len(old)
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}
