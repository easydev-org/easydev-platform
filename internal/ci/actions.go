package ci

import (
	"fmt"
	"path"
	"strings"

	"sigs.k8s.io/yaml"
)

// Action resolution: `uses: <namespace>/<path>@<ref>`.
//
// The syntax is GitHub's, but the MEANING falls straight out of the VCS model:
// a namespace is the container and any directory is a scope, so an action is
// just a subtree at a pinned ref.
//
//	uses: easydev-org/ci-actions/setup-go@v3
//	      └─────┬─────┘ └─────┬────┘ └┬┘
//	       namespace        path      ref
//
// The subtree archive endpoint already exists (`/archive/{ref}.tar.gz?path=`),
// so the executor can materialise an action with no extra service and no
// clone cache.
//
// Two action kinds are supported, both resolved AT LOAD TIME into a shell
// script so the executor only ever runs one container per step:
//
//	run     — an action.yml with `runs: {using: "shell", script: "..."}`
//	composite — an action.yml with a list of steps to inline
//
// A JavaScript/Docker action runtime is deliberately NOT implemented: it is a
// large attack surface for little gain, and composite covers the same ground.

// ActionRef is a parsed `uses:` value.
type ActionRef struct {
	// Raw is the original string.
	Raw string
	// Namespace is the action's owning namespace ("" for a local ./path ref).
	Namespace string
	// Path is the action's directory inside the namespace ("" for a local ref,
	// where Path is the relative path on disk).
	Path string
	// Ref is the pinned ref (branch, tag, revision id or snapshot sha).
	Ref string
	// Local is true for `./path/to/action` (an action in the SAME checkout).
	Local bool
}

// ParseActionRef parses `ns/path@ref` or `./path`.
func ParseActionRef(s string) (*ActionRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("ci: empty action reference")
	}
	if strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return &ActionRef{Raw: s, Path: s, Local: true}, nil
	}
	body, ref, ok := strings.Cut(s, "@")
	if !ok {
		return nil, fmt.Errorf("ci: action %q has no @ref (a pinned ref is required)", s)
	}
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("ci: action %q has an empty ref", s)
	}
	ns, rest, ok := strings.Cut(strings.Trim(body, "/"), "/")
	if !ok || ns == "" {
		return nil, fmt.Errorf("ci: action %q must be <namespace>/<path>@<ref>", s)
	}
	// A two-segment form `ns/name@ref` is a whole-namespace action (path "").
	if rest == "" {
		return &ActionRef{Raw: s, Namespace: ns, Ref: ref}, nil
	}
	return &ActionRef{Raw: s, Namespace: ns, Path: strings.Trim(rest, "/"), Ref: ref}, nil
}

// ArchiveQuery returns the subtree path to fetch ("" = whole namespace).
func (a *ActionRef) ArchiveQuery() string { return a.Path }

// String renders the reference back.
func (a *ActionRef) String() string {
	if a.Local {
		return a.Path
	}
	if a.Path == "" {
		return a.Namespace + "@" + a.Ref
	}
	return a.Namespace + "/" + a.Path + "@" + a.Ref
}

// actionFile is the parsed action.yml.
type actionFile struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	Inputs      map[string]rawInput  `json:"inputs"`
	Outputs     map[string]rawOutput `json:"outputs"`
	Runs        rawRuns              `json:"runs"`
}

type rawInput struct {
	Description string `json:"description"`
	Default     string `json:"default"`
	Required    bool   `json:"required"`
}

type rawOutput struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

type rawRuns struct {
	Using  string    `json:"using"`
	Script string    `json:"script"`
	Shell  string    `json:"shell"`
	Main   string    `json:"main"`
	Image  string    `json:"image"`
	Steps  []rawStep `json:"steps"`
}

// Action is a resolved action: the shell script to run plus its outputs.
type Action struct {
	Ref  *ActionRef
	Name string
	// Script is the shell to run (a `run` action's script, or the inlined
	// composite steps joined with `set -e` between them).
	Script string
	// Env carries the action's declared inputs, already defaulted and
	// interpolated into the INPUT_* variables, plus any action-scoped env.
	Env map[string]string
	// Outputs maps an output name to a shell expansion performed at the end of
	// Script (composite: `steps.<id>.outputs.<k>`, shell: a command).
	Outputs map[string]string
	// Steps is the composite's step list (kept for diagnostics).
	Steps []Step
}

// ActionResolver loads an action definition by its reference. The platform
// implements it on top of easyvcs (subtree archive at a pinned ref); tests use
// an in-memory map.
type ActionResolver interface {
	// Load returns the action.yml bytes for a reference. `nil, false` means the
	// action does not exist (a 404) — never a network error.
	Load(ref *ActionRef) (data []byte, found bool, err error)
}

// MapActionResolver is an in-memory ActionResolver (tests, and the gateway's
// per-run cache).
type MapActionResolver map[string][]byte

// Load implements ActionResolver.
func (m MapActionResolver) Load(ref *ActionRef) ([]byte, bool, error) {
	if b, ok := m[ref.String()]; ok {
		return b, true, nil
	}
	if b, ok := m[ref.Namespace+"/"+ref.Path+"@"+ref.Ref]; ok {
		return b, true, nil
	}
	return nil, false, nil
}

// resolverFor is the process-wide resolver, set once at boot by main.
var resolverFor ActionResolver

// SetActionResolver installs the process-wide action resolver.
func SetActionResolver(r ActionResolver) { resolverFor = r }

// CurrentActionResolver returns the installed resolver (nil = none).
func CurrentActionResolver() ActionResolver { return resolverFor }

// ResolveAction resolves a `uses:` value into an executable Action using the
// installed resolver.
func ResolveAction(uses string, job *WorkflowJob, st *Step) (*Action, error) {
	return ResolveActionWith(resolverFor, uses, job, st)
}

// ResolveActionWith is ResolveAction with an explicit resolver.
func ResolveActionWith(r ActionResolver, uses string, job *WorkflowJob, st *Step) (*Action, error) {
	ref, err := ParseActionRef(uses)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("ci: `uses: %s` needs an action resolver (none configured)", uses)
	}
	data, found, err := r.Load(ref)
	if err != nil {
		return nil, fmt.Errorf("ci: load action %s: %w", uses, err)
	}
	if !found {
		return nil, fmt.Errorf("ci: action %s not found", uses)
	}
	return CompileAction(ref, data, st.With)
}

// CompileAction turns action.yml bytes into the shell form.
func CompileAction(ref *ActionRef, data []byte, with map[string]string) (*Action, error) {
	var af actionFile
	if err := yaml.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("ci: action %s: parse action.yml: %w", ref, err)
	}
	env := map[string]string{}
	// Inputs: every declared input becomes INPUT_<NAME>, defaulted, and an
	// undeclared `with:` key is an error (a typo must not silently do nothing).
	seen := map[string]bool{}
	for name, in := range af.Inputs {
		seen[strings.ToUpper(name)] = true
		v := in.Default
		if got, ok := with[name]; ok {
			v = got
		} else if got, ok := with[strings.ToUpper(name)]; ok {
			v = got
		}
		if in.Required && strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("ci: action %s: input %q is required", ref, name)
		}
		env["INPUT_"+strings.ToUpper(strings.ReplaceAll(name, " ", "_"))] = v
	}
	for k := range with {
		if !seen[strings.ToUpper(k)] {
			return nil, fmt.Errorf("ci: action %s: unknown input %q", ref, k)
		}
	}

	out := &Action{Ref: ref, Name: firstNonEmpty(af.Name, ref.Path, ref.Namespace), Env: env}

	switch strings.ToLower(af.Runs.Using) {
	case "shell", "sh", "bash":
		if strings.TrimSpace(af.Runs.Script) == "" {
			return nil, fmt.Errorf("ci: action %s: runs.script is empty", ref)
		}
		out.Script = af.Runs.Script
	case "composite":
		if len(af.Runs.Steps) == 0 {
			return nil, fmt.Errorf("ci: action %s: composite has no steps", ref)
		}
		var b strings.Builder
		for _, rs := range af.Runs.Steps {
			if rs.Run == "" {
				return nil, fmt.Errorf("ci: action %s: composite step %q uses %q (nested `uses` is not supported: inline it)", ref, rs.Name, rs.Uses)
			}
			out.Steps = append(out.Steps, Step{ID: rs.ID, Name: rs.Name, Run: rs.Run, Shell: rs.Shell})
			b.WriteString("echo '::easydev:step ")
			b.WriteString(sanitize(firstNonEmpty(rs.Name, rs.ID, "step")))
			b.WriteString("'\n")
			b.WriteString(rs.Run)
			b.WriteString("\n")
		}
		out.Script = b.String()
	case "docker", "node", "javascript", "nodejs":
		return nil, fmt.Errorf("ci: action %s: `runs.using: %s` is not supported (use a composite or a shell action)", ref, af.Runs.Using)
	case "":
		return nil, fmt.Errorf("ci: action %s: action.yml has no runs.using", ref)
	default:
		return nil, fmt.Errorf("ci: action %s: unknown runs.using %q", ref, af.Runs.Using)
	}

	// Outputs: `<name> -> shell expression` evaluated after the script. The
	// supported form is `${{ steps.<id>.outputs.<k> }}`, which the executor
	// resolves from the step output file.
	for name, o := range af.Outputs {
		out.Outputs[name] = o.Value
	}
	return out, nil
}

// ActionPathInNamespace is the action's directory (used to build archive URLs).
func ActionPathInNamespace(a *ActionRef) string {
	if a.Local {
		return path.Clean(a.Path)
	}
	if a.Path == "" {
		return "."
	}
	return a.Path
}
