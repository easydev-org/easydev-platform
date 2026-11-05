package ci

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// stubMinter records minted tokens.
type stubMinter struct {
	mu    sync.Mutex
	calls []mintCall
	// failOn makes a mint attempt fail (to exercise the error path).
	failOn string
}

type mintCall struct {
	Tenant string
	Label  string
	Write  bool
}

func (m *stubMinter) MintToken(ctx context.Context, tenant, label string, write bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failOn != "" && strings.Contains(label, m.failOn) {
		return "", fmt.Errorf("boom")
	}
	m.calls = append(m.calls, mintCall{Tenant: tenant, Label: label, Write: write})
	if write {
		return "write-token", nil
	}
	return "read-token", nil
}

// TestPermissionsDefaultToNone: a workflow that declares nothing gets no
// credential at all.
func TestPermissionsDefaultToNone(t *testing.T) {
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j: {container: {image: busybox}, steps: [{run: "echo hi"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	perms, err := wf.EffectivePermissions()
	if err != nil {
		t.Fatal(err)
	}
	for scope, p := range perms {
		if p != PermNone {
			t.Errorf("%s = %s, want none", scope, p)
		}
	}
}

// TestPermissionsRejectUnknownScope: an unknown key is a hard error, not a
// silent no-op (which would give a false sense of restriction).
func TestPermissionsRejectUnknownScope(t *testing.T) {
	_, err := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  security-events: write
jobs:
  j: {container: {image: busybox}, steps: [{run: "echo hi"}]}
`))
	// Parsing accepts the map; VALIDATION rejects it (the workflow must be
	// loadable, but it must not RUN with an unknown scope).
	wf, perr := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  security-events: write
jobs:
  j: {container: {image: busybox}, steps: [{run: "echo hi"}]}
`))
	_ = err
	if perr != nil {
		return // rejected at load time: also acceptable
	}
	if _, verr := wf.EffectivePermissions(); verr == nil {
		t.Fatal("an unknown permission scope must be rejected")
	}
}

// TestPermissionsRejectBadLevel.
func TestPermissionsRejectBadLevel(t *testing.T) {
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  contents: admin
jobs:
  j: {container: {image: busybox}, steps: [{run: "echo hi"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.EffectivePermissions(); err == nil {
		t.Fatal("an invalid permission level must be rejected")
	}
}

// TestReadOnlyPermissionMintsAReadToken: `contents: read` grants a read token and
// NOT the write one.
func TestReadOnlyPermissionMintsAReadToken(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	minter := &stubMinter{}
	w := &recordingWriter{}
	o := NewOrchestrator(st, fops).WithTokenMinter(minter).WithSecrets(mapSecrets{}, w)

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  contents: read
jobs:
  j:
    container: {image: busybox}
    steps: [{run: "cat $EASYDEV_VCS_TOKEN"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)

	if len(minter.calls) != 1 {
		t.Fatalf("mint calls = %+v", minter.calls)
	}
	if minter.calls[0].Write {
		t.Error("`contents: read` must mint a READ token")
	}
	if minter.calls[0].Tenant != "team-a" {
		t.Errorf("token tenant = %q", minter.calls[0].Tenant)
	}
	// The read token is injected, the write token is NOT.
	refs := fops.Specs()[0].SecretEnv
	var haveRead, haveWrite bool
	for _, r := range refs {
		switch r.Env {
		case EnvVCSReadToken:
			haveRead = true
		case EnvVCSWriteToken:
			haveWrite = true
		}
	}
	if !haveRead {
		t.Error("the read token was not injected")
	}
	if haveWrite {
		t.Error("`contents: read` must NOT inject the write token")
	}
	// And no literal value in the spec.
	if strings.Contains(fops.Specs()[0].Commands[0], "read-token") {
		t.Error("the token value leaked into the script")
	}
}

// TestWritePermissionGrantsBothTokens.
func TestWritePermissionGrantsBothTokens(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	minter := &stubMinter{}
	o := NewOrchestrator(st, fops).WithTokenMinter(minter).
		WithSecrets(mapSecrets{}, &recordingWriter{})

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  contents: write
jobs:
  j:
    container: {image: busybox}
    steps: [{run: "echo hi"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)

	if len(minter.calls) != 1 || !minter.calls[0].Write {
		t.Fatalf("mint calls = %+v, want one write token", minter.calls)
	}
	var haveWrite bool
	for _, r := range fops.Specs()[0].SecretEnv {
		if r.Env == EnvVCSWriteToken {
			haveWrite = true
		}
	}
	if !haveWrite {
		t.Error("`contents: write` must inject the write token")
	}
}

// TestRequestedPermissionWithoutAMinterFails: a workflow must not run with less
// than it declared.
func TestRequestedPermissionWithoutAMinterFails(t *testing.T) {
	st := NewMemStore()
	o := NewOrchestrator(st, NewFakeOps()) // no minter
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  contents: write
jobs:
  j:
    container: {image: busybox}
    steps: [{run: "echo hi"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Head: "h", Workflow: wf,
	}); err == nil {
		t.Fatal("a requested permission the platform cannot grant must fail the run")
	}
}

// TestNoPermissionMeansNoToken: an undeclared workflow never mints.
func TestNoPermissionMeansNoToken(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	minter := &stubMinter{}
	o := NewOrchestrator(st, fops).WithTokenMinter(minter)
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps: [{run: "echo hi"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)
	if len(minter.calls) != 0 {
		t.Fatalf("an undeclared workflow minted tokens: %+v", minter.calls)
	}
	if n := len(fops.Specs()[0].SecretEnv); n != 0 {
		t.Errorf("secret refs = %d, want 0", n)
	}
}
