package wf

import (
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// fixture builds an in-memory easyvcs namespace with a file tree.
func fixture(t *testing.T, files map[string]string) (*Source, *store.Repo, string) {
	t.Helper()
	cs, err := store.OpenDriver(store.DriverConfig{Kind: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	u, err := cs.CreateUser("alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.NSRef{Name: "team", Owner: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	ws := revision.NewWorkspace(repo)

	// Build a tree from an in-memory map by committing file changes.
	specs := make([]revision.FileChangeSpec, 0, len(files))
	for path, content := range files {
		specs = append(specs, revision.FileChangeSpec{
			Path: path, Content: []byte(content),
		})
	}
	snap, _, err := ws.CommitFromChanges(object.ID{}, specs, "seed", store.Author{Name: "alice"}, "")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, err := repo.GetSnapshot(snap.RevisionHash); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// A real namespace has a default branch; the resolver addresses refs by it.
	if err := repo.PutRef(&store.Ref{
		Name: "main", Kind: store.RefBranch, Target: snap.RevisionID, OwnerUserID: 0,
	}); err != nil {
		t.Fatalf("put ref: %v", err)
	}
	return New(cs), repo, snap.RevisionHash.String()
}

func TestLoadWorkflowFromSnapshot(t *testing.T) {
	src, _, sha := fixture(t, map[string]string{
		".easydev/workflows/ci.yml": `
on: [rr_opened]
jobs:
  t: {image: busybox, commands: ["true"]}
`,
	})
	w, path, err := src.Workflow("team", sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if path != ci.DefaultWorkflowPath {
		t.Fatalf("path = %q", path)
	}
	if len(w.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(w.Jobs))
	}
}

func TestLoadWorkflowMissing(t *testing.T) {
	src, _, sha := fixture(t, map[string]string{"README.md": "x"})
	if _, _, err := src.Workflow("team", sha, ""); err == nil {
		t.Fatal("a missing workflow must be an error")
	}
}

func TestLoadWorkflowAtRootPath(t *testing.T) {
	src, _, sha := fixture(t, map[string]string{
		".easydev-ci.yml": "on: [rr_opened]\njobs:\n  t: {image: x, commands: [y]}\n",
	})
	// A bare path is tried with .yml / .yaml suffixes.
	w, got, err := src.Workflow("team", sha, ".easydev-ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	if got != ".easydev-ci.yml" || len(w.Jobs) != 1 {
		t.Fatalf("path=%q jobs=%d", got, len(w.Jobs))
	}
}

func TestListWorkflows(t *testing.T) {
	src, _, sha := fixture(t, map[string]string{
		".easydev/workflows/ci.yml":      "on: [rr_opened]\njobs:\n  a: {image: x, commands: [y]}\n",
		".easydev/workflows/release.yml": "on: [tag]\njobs:\n  b: {image: x, commands: [y]}\n",
		".easydev/workflows/notes.md":    "ignore me",
	})
	got, err := src.ListWorkflows("team", sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("workflows = %v", got)
	}
}

func TestActionLoad(t *testing.T) {
	src, _, _ := fixture(t, map[string]string{
		".easydev/workflows/ci.yml": "on: [rr_opened]\njobs:\n  a: {image: x, commands: [y]}\n",
		"actions/setup-go/action.yml": `
name: setup-go
inputs:
  go-version: {default: "1.27"}
runs:
  using: shell
  script: echo "installing $INPUT_GO_VERSION"
`,
	})
	sha, err := src.ResolveSnapshot("team", "")
	if err != nil {
		t.Fatal(err)
	}
	ref := &ci.ActionRef{Namespace: "team", Path: "actions/setup-go", Ref: sha}
	data, found, err := src.Load(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("action not found")
	}
	a, err := ci.CompileAction(ref, data, map[string]string{"go-version": "1.26"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Env["INPUT_GO-VERSION"] != "1.26" {
		t.Fatalf("inputs = %v", a.Env)
	}
}

func TestActionNotFound(t *testing.T) {
	src, _, _ := fixture(t, map[string]string{"x.txt": "x"})
	sha, _ := src.ResolveSnapshot("team", "")
	_, found, err := src.Load(&ci.ActionRef{Namespace: "team", Path: "nope", Ref: sha})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a missing action must not be found")
	}
}
