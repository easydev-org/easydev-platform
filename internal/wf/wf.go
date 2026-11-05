// Package wf resolves workflows and actions OUT of easyvcs.
//
// It is the only place in the platform that reads VCS content for CI, and it
// enforces the two rules that make CI safe:
//
//  1. A workflow is read from a PINNED SNAPSHOT, never from a request body. A
//     run therefore cannot be steered by whoever posts the event.
//  2. An action is read from a subtree at a pinned ref, so `uses:` costs no
//     clone, no cache and no extra service — and a tag can be immutable
//     (easyvcs tags cannot be re-pointed).
package wf

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// Source reads workflows and actions from one workspace.
type Source struct {
	CS *store.CentralStore
}

// New returns a resolver over the given central store.
func New(cs *store.CentralStore) *Source { return &Source{CS: cs} }

// ErrNotFound is returned when the file or ref does not exist.
var ErrNotFound = errors.New("wf: not found")

// open opens a namespace (the single container) for reading.
func (s *Source) open(ns string) (*store.Repo, *revision.Workspace, error) {
	if strings.TrimSpace(ns) == "" {
		return nil, nil, fmt.Errorf("wf: namespace is required")
	}
	repo, err := s.CS.OpenRepo(store.NSRef{Name: ns})
	if err != nil {
		return nil, nil, fmt.Errorf("wf: open namespace %q: %w", ns, err)
	}
	return repo, revision.NewWorkspace(repo), nil
}

// ResolveSnapshot turns a ref expression (branch, tag, revision id/prefix,
// snapshot sha) into a snapshot sha. Empty means the default branch.
func (s *Source) ResolveSnapshot(ns, ref string) (string, error) {
	repo, _, err := s.open(ns)
	if err != nil {
		return "", err
	}
	return s.resolveSnapshot(repo, ref)
}

func (s *Source) resolveSnapshot(repo *store.Repo, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "main"
		if meta, err := repo.RepoMeta(); err == nil && meta.DefaultBranch != "" {
			ref = meta.DefaultBranch
		}
	}
	// A snapshot sha can be addressed directly.
	if id, err := object.HexToID(ref); err == nil {
		if _, err := repo.GetSnapshot(id); err == nil {
			return id.String(), nil
		}
	}
	// A branch or tag.
	if r, err := repo.GetRef(ref); err == nil && r != nil {
		if rev, err := repo.GetRevision(r.Target); err == nil {
			return rev.Hash.String(), nil
		}
	}
	// A revision id (or a unique prefix).
	if rev, err := repo.GetRevision(ref); err == nil {
		return rev.Hash.String(), nil
	}
	if revs, err := repo.ListRevisions(); err == nil {
		for _, r := range revs {
			if strings.HasPrefix(r.ID, ref) {
				return r.Hash.String(), nil
			}
		}
		for _, r := range revs {
			if r.Hash.String() == ref {
				return r.Hash.String(), nil
			}
		}
	}
	return "", fmt.Errorf("%w: unknown ref %q", ErrNotFound, ref)
}

// readFile reads a file at a path from a snapshot. `path` is slash-separated
// and repo-relative.
func (s *Source) readFile(repo *store.Repo, ws *revision.Workspace, sha, path string) ([]byte, error) {
	id, err := object.HexToID(sha)
	if err != nil {
		return nil, fmt.Errorf("wf: bad snapshot %q: %w", sha, err)
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot %s", ErrNotFound, sha)
	}
	root, err := ws.ReadTree(snap.TreeID)
	if err != nil {
		return nil, err
	}
	e, err := ws.FindEntry(root, strings.Trim(path, "/"))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if e.Kind != object.KindBlob {
		return nil, fmt.Errorf("wf: %s is not a file", path)
	}
	return ws.ReadBlob(e.ID)
}

// workflowCandidates are the paths a workflow may live at, in priority order.
func workflowCandidates(path string) []string {
	p := strings.Trim(strings.TrimSpace(path), "/")
	if p == "" {
		return []string{ci.DefaultWorkflowPath}
	}
	if strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml") {
		return []string{p}
	}
	return []string{p, p + ".yml", p + ".yaml"}
}

// Workflow loads and parses the workflow at `path` from snapshot `sha`.
func (s *Source) Workflow(ns, sha, path string) (*ci.Workflow, string, error) {
	repo, ws, err := s.open(ns)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(sha) == "" {
		if sha, err = s.resolveSnapshot(repo, ""); err != nil {
			return nil, "", err
		}
	}
	var lastErr error
	for _, cand := range workflowCandidates(path) {
		data, err := s.readFile(repo, ws, sha, cand)
		if err != nil {
			lastErr = err
			continue
		}
		w, perr := ci.ParseWorkflow(data)
		if perr != nil {
			return nil, cand, fmt.Errorf("wf: %s@%s: %w", cand, short(sha), perr)
		}
		w.Source = cand
		return w, cand, nil
	}
	if lastErr == nil {
		lastErr = ErrNotFound
	}
	return nil, "", fmt.Errorf("wf: workflow %q at %s: %w", path, short(sha), lastErr)
}

// ListWorkflows returns every `*.yml`/`*.yaml` directly under the workflow
// directory of a snapshot, sorted.
func (s *Source) ListWorkflows(ns, sha string) ([]string, error) {
	repo, ws, err := s.open(ns)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sha) == "" {
		if sha, err = s.resolveSnapshot(repo, ""); err != nil {
			return nil, err
		}
	}
	id, err := object.HexToID(sha)
	if err != nil {
		return nil, err
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		return nil, fmt.Errorf("%w: snapshot %s", ErrNotFound, sha)
	}
	root, err := ws.ReadTree(snap.TreeID)
	if err != nil {
		return nil, err
	}
	dir, err := ws.FindEntry(root, ci.WorkflowDir)
	if err != nil || dir.Kind != object.KindTree {
		return nil, nil // no workflows directory: not an error
	}
	tree, err := ws.ReadTree(dir.ID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range tree.SortedEntries() {
		if e.Kind != object.KindBlob {
			continue
		}
		if strings.HasSuffix(e.Name, ".yml") || strings.HasSuffix(e.Name, ".yaml") {
			out = append(out, ci.WorkflowDir+"/"+e.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// HasWorkflow reports whether a snapshot carries a workflow at path.
func (s *Source) HasWorkflow(ns, sha, path string) bool {
	_, _, err := s.Workflow(ns, sha, path)
	return err == nil
}

// Load implements ci.ActionResolver: it reads `action.yml` (or `action.yaml`)
// from the action's subtree at its ref.
func (s *Source) Load(ref *ci.ActionRef) ([]byte, bool, error) {
	if ref.Local {
		// A local `./path` action lives in the caller's checkout; the executor
		// resolves it from the workspace at run time, so there is nothing to
		// fetch here. Signal "handled by the executor" with an empty definition
		// only when a loader exists for it.
		return nil, false, nil
	}
	repo, ws, err := s.open(ref.Namespace)
	if err != nil {
		return nil, false, err
	}
	sha, err := s.resolveSnapshot(repo, ref.Ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	base := strings.Trim(ref.Path, "/")
	for _, name := range []string{"action.yml", "action.yaml"} {
		p := name
		if base != "" {
			p = base + "/" + name
		}
		data, rerr := s.readFile(repo, ws, sha, p)
		if rerr == nil {
			return data, true, nil
		}
		if !errors.Is(rerr, ErrNotFound) {
			return nil, false, rerr
		}
	}
	return nil, false, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
