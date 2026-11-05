package ci

import (
	"context"
	"strings"
	"testing"
)

func concWorkflow(group string, cancel bool) *Workflow {
	src := `
concurrency:
  group: "` + group + `"
  cancel-in-progress: ` + map[bool]string{true: "true", false: "false"}[cancel] + `
on: [rr_opened]
jobs:
  t:
    container: {image: busybox}
    steps: [{run: "true"}]
`
	w, err := ParseWorkflow([]byte(src))
	if err != nil {
		panic(err)
	}
	return w
}

// TestConcurrencyGroupResolvesFromTheContext: the template substitutes the
// easyvcs context and is then sanitised into a label.
func TestConcurrencyGroupResolvesFromTheContext(t *testing.T) {
	run := &Run{Namespace: "team-a", Ref: "feature/x", RR: 12, Trigger: TriggerRROpened}
	got := GroupOf("ci-${{ easyvcs.namespace }}-${{ easyvcs.rr }}", run)
	want := "ci-team-a-12"
	if got != want {
		t.Fatalf("group = %q, want %q", got, want)
	}
	// A branch name with a slash must still be a legal label.
	if got := GroupOf("${{ easyvcs.ref }}", run); strings.Contains(got, "/") {
		t.Fatalf("group %q still contains a slash", got)
	}
	if got := GroupOf("", run); got != "" {
		t.Fatalf("an empty template must yield an empty group, got %q", got)
	}
}

// TestConcurrencyRefusesWhenBusy: without cancel-in-progress a second run in the
// same group is refused.
func TestConcurrencyRefusesWhenBusy(t *testing.T) {
	st := NewMemStore()
	o := NewOrchestrator(st, NewFakeOps()).WithConcurrency(NewConcurrencyGate(st))
	wf := concWorkflow("ci-${{ easyvcs.ref }}", false)

	first, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "h1", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The first run may still be running (it is fast), so make it durable by
	// re-writing it as running.
	r, _ := st.GetRun(context.Background(), first)
	r.State = RunRunning
	_ = st.PutRun(context.Background(), r)

	_, err = o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "h2", Workflow: wf,
	})
	if err == nil || !strings.Contains(err.Error(), "concurrency group") {
		t.Fatalf("second run = %v, want a group-busy error", err)
	}
}

// TestConcurrencyCancelsInProgress: with cancel-in-progress the older run is
// cancelled and the new one proceeds.
func TestConcurrencyCancelsInProgress(t *testing.T) {
	st := NewMemStore()
	o := NewOrchestrator(st, NewFakeOps()).WithConcurrency(NewConcurrencyGate(st))
	wf := concWorkflow("ci-${{ easyvcs.ref }}", true)

	first, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "h1", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := st.GetRun(context.Background(), first)
	r.State = RunRunning
	_ = st.PutRun(context.Background(), r)

	second, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "h2", Workflow: wf,
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second == first {
		t.Fatal("the second run must get a new id")
	}
	old, _ := st.GetRun(context.Background(), first)
	if old.State != RunCanceled {
		t.Fatalf("the superseded run = %s, want canceled", old.State)
	}
}

// TestConcurrencyDifferentGroupsDoNotCollide.
func TestConcurrencyDifferentGroupsDoNotCollide(t *testing.T) {
	st := NewMemStore()
	o := NewOrchestrator(st, NewFakeOps()).WithConcurrency(NewConcurrencyGate(st))
	wf := concWorkflow("ci-${{ easyvcs.ref }}", false)

	if _, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "h1", Workflow: wf,
	}); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.ListRuns(context.Background(), "team-a", 10)
	runs[0].State = RunRunning
	_ = st.PutRun(context.Background(), runs[0])

	// A different ref is a different group.
	if _, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "release", Head: "h2", Workflow: wf,
	}); err != nil {
		t.Fatalf("a different group must not be blocked: %v", err)
	}
}

// TestGroupGateIsCrossReplica: a SQLStore answers the group query from the
// shared database, so two orchestrators over the same DB agree.
func TestGroupGateIsCrossReplica(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// A live run in group "g" written by "replica 1".
	_ = s.PutRun(ctx, &Run{ID: "other", Namespace: "a", State: RunRunning, Group: "g", Created: 1})

	gate := NewConcurrencyGate(s)
	run := &Run{ID: "new", Namespace: "a", Group: "g"}
	if _, err := gate.Acquire(ctx, "g", run, false); err != ErrGroupBusy {
		t.Fatalf("acquire = %v, want ErrGroupBusy", err)
	}
	// A terminal run in the group does not block.
	_ = s.PutRun(ctx, &Run{ID: "done", Namespace: "a", State: RunSuccess, Group: "g2", Created: 1})
	if _, err := gate.Acquire(ctx, "g2", &Run{ID: "n2", Group: "g2"}, false); err != nil {
		t.Fatalf("a finished run must not block: %v", err)
	}
}

// TestConcurrencyNoGroupIsAutoPass: a workflow without `concurrency:` is never
// gated.
func TestConcurrencyNoGroupIsAutoPass(t *testing.T) {
	st := NewMemStore()
	o := NewOrchestrator(st, NewFakeOps()).WithConcurrency(NewConcurrencyGate(st))
	wf, err := ParseWorkflow([]byte(sampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := o.Run(context.Background(), Event{
			Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "h", Workflow: wf,
		}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
}
