package ci

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// rawWorkflow mirrors the on-disk workflow shape. It is intentionally close to
// GitHub Actions, so a team can migrate a workflow with small edits:
//
//	version: 1
//	on:
//	  rr: { types: [opened, synced] }
//	jobs:
//	  test:
//	    runs-on: [linux]
//	    steps:
//	      - uses: easydev-org/ci-actions/checkout@v3
//	      - run: go test ./...
//
// The v1 form (`jobs.<id>.commands`) is still accepted: it becomes one `run`
// step whose script is the commands joined by newlines.
type rawWorkflow struct {
	Version     int                `json:"version"`
	Name        string             `json:"name"`
	On          yamlOn             `json:"on"`
	Permissions map[string]string  `json:"permissions"`
	Concurrency *rawConcurrency    `json:"concurrency"`
	Env         map[string]string  `json:"env"`
	Jobs        map[string]*rawJob `json:"jobs"`
}

// yamlOn accepts every `on:` spelling that is seen in the wild:
//
//	on: [rr_opened, rr_synced]                       (list of trigger names)
//	on: rr_opened                                     (one name)
//	on: { rr: {types: [...]}, tag: {pattern: v*} }    (the full form)
//	on: { push: ... }                                 (GitHub's names, mapped)
//
// YAML 1.1 parses a bare `on` as the boolean true, which is exactly why this is
// a custom unmarshaller.
type yamlOn struct {
	RR       *RREvent
	Tag      *TagEvent
	Manual   bool
	Schedule []string
}

func (o *yamlOn) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if trimmed[0] == '[' || trimmed[0] == '"' || trimmed[0] == '\'' || (trimmed[0] >= '0' && trimmed[0] <= '9') || trimmed == "true" || trimmed == "false" {
		var one any
		if err := yaml.Unmarshal(b, &one); err != nil {
			return err
		}
		return o.applyShorthand(one)
	}
	if trimmed[0] == '{' {
		var m map[string]any
		if err := yaml.Unmarshal(b, &m); err != nil {
			return err
		}
		return o.applyMap(m)
	}
	var s string
	if err := yaml.Unmarshal(b, &s); err != nil {
		return err
	}
	return o.applyShorthand(s)
}

func (o *yamlOn) applyShorthand(v any) error {
	switch x := v.(type) {
	case bool:
		// `on: true` is what a bare `on:` key degenerates into under YAML 1.1.
		// Treat it as "the default RR triggers" rather than an error.
		if x {
			o.RR = &RREvent{}
		}
		return nil
	case []any:
		names := make([]string, 0, len(x))
		for _, e := range x {
			names = append(names, fmt.Sprint(e))
		}
		return o.applyNames(names)
	case string:
		return o.applyNames([]string{x})
	}
	return fmt.Errorf("ci: unsupported `on:` value %T", v)
}

func (o *yamlOn) applyNames(names []string) error {
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "rr", "rr_opened", "rr.opened", "opened", "pull_request", "pull_request_target":
			if o.RR == nil {
				o.RR = &RREvent{}
			}
			if t := rrTypeOfName(n); t != "" && !containsFold(o.RR.Types, t) {
				o.RR.Types = append(o.RR.Types, t)
			}
		case "rr_synced", "rr.synced", "synced", "updated", "synchronize":
			if o.RR == nil {
				o.RR = &RREvent{}
			}
			if !containsFold(o.RR.Types, "synced") {
				o.RR.Types = append(o.RR.Types, "synced")
			}
		case "rr_merged", "rr.merged", "merged", "closed":
			if o.RR == nil {
				o.RR = &RREvent{}
			}
			if !containsFold(o.RR.Types, "merged") {
				o.RR.Types = append(o.RR.Types, "merged")
			}
		case "tag", "tag_created", "tag.created", "push_tag":
			if o.Tag == nil {
				o.Tag = &TagEvent{}
			}
		case "manual", "workflow_dispatch", "dispatch":
			o.Manual = true
		case "schedule", "cron":
			o.Schedule = append(o.Schedule, "") // validated in applyMap
		default:
			return fmt.Errorf("ci: unknown trigger %q", n)
		}
	}
	return nil
}

func rrTypeOfName(n string) string {
	switch strings.ToLower(strings.TrimSpace(n)) {
	case "rr", "rr_opened", "rr.opened", "opened", "pull_request", "pull_request_target":
		return "opened"
	}
	return ""
}

func (o *yamlOn) applyMap(m map[string]any) error {
	for key, val := range m {
		switch strings.ToLower(key) {
		case "rr", "pull_request", "pull_request_target", "push":
			rr := &RREvent{}
			if sub, ok := val.(map[string]any); ok {
				if types, ok := sub["types"]; ok {
					for _, t := range toStrings(types) {
						if name := rrTypeOfName(t); name != "" {
							rr.Types = append(rr.Types, name)
							continue
						}
						switch strings.ToLower(t) {
						case "synced", "synchronize", "updated":
							rr.Types = append(rr.Types, "synced")
						case "merged", "closed":
							rr.Types = append(rr.Types, "merged")
						default:
							return fmt.Errorf("ci: unknown rr type %q", t)
						}
					}
				}
				rr.Paths = toStrings(sub["paths"])
				rr.Branches = toStrings(sub["branches"])
				if s, ok := sub["base"].(string); ok {
					rr.Base = s
				}
			}
			o.RR = rr
		case "tag", "tag_created", "tag.created":
			t := &TagEvent{}
			if sub, ok := val.(map[string]any); ok {
				if s, ok := sub["pattern"].(string); ok {
					t.Pattern = s
				}
			} else if s, ok := val.(string); ok {
				t.Pattern = s
			}
			o.Tag = t
		case "manual", "workflow_dispatch", "dispatch":
			o.Manual = true
		case "schedule":
			for _, s := range toStrings(val) {
				if strings.TrimSpace(s) != "" {
					o.Schedule = append(o.Schedule, strings.TrimSpace(s))
				}
			}
		default:
			return fmt.Errorf("ci: unknown trigger %q", key)
		}
	}
	// Drop the empty schedule marker produced by the shorthand form.
	kept := o.Schedule[:0]
	for _, s := range o.Schedule {
		if s != "" {
			kept = append(kept, s)
		}
	}
	o.Schedule = kept
	return nil
}

type rawConcurrency struct {
	Group            string `json:"group"`
	CancelInProgress bool   `json:"cancel-in-progress"`
}

type rawJob struct {
	Name      string                 `json:"name"`
	Needs     []string               `json:"needs"`
	If        string                 `json:"if"`
	RunsOn    []string               `json:"runs-on"`
	Strategy  *rawStrategy           `json:"strategy"`
	Container *rawContainer          `json:"container"`
	Services  map[string]*rawService `json:"services"`
	Env       map[string]string      `json:"env"`
	Steps     []rawStep              `json:"steps"`
	// v1 aliases: a job with commands produces a single run step.
	Commands        []string          `json:"commands"`
	Script          []string          `json:"script"`
	Image           string            `json:"image"`
	CPU             string            `json:"cpu"`
	Memory          string            `json:"memory"`
	TimeoutSeconds  int               `json:"timeout_seconds"`
	TimeoutMinutes  int               `json:"timeout-minutes"`
	Artifacts       []string          `json:"artifacts"`
	Outputs         map[string]string `json:"outputs"`
	Required        bool              `json:"required"`
	ContinueOnError bool              `json:"continue-on-error"`
}

type rawStrategy struct {
	Matrix      map[string]any `json:"matrix"`
	FailFast    *bool          `json:"fail-fast"`
	MaxParallel int            `json:"max-parallel"`
}

type rawContainer struct {
	Image   string            `json:"image"`
	Env     map[string]string `json:"env"`
	Command []string          `json:"command"`
	Args    []string          `json:"args"`
}

type rawService struct {
	Image   string            `json:"image"`
	Env     map[string]string `json:"env"`
	Ports   []int             `json:"ports"`
	Command []string          `json:"command"`
	Args    []string          `json:"args"`
}

type rawStep struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	If               string            `json:"if"`
	Uses             string            `json:"uses"`
	With             map[string]string `json:"with"`
	Run              string            `json:"run"`
	Shell            string            `json:"shell"`
	Env              map[string]string `json:"env"`
	WorkingDirectory string            `json:"working-directory"`
	ContinueOnError  bool              `json:"continue-on-error"`
	TimeoutMinutes   int               `json:"timeout-minutes"`
}

// ParseWorkflow parses and validates a workflow file.
func ParseWorkflow(data []byte) (*Workflow, error) {
	var raw rawWorkflow
	// YAML 1.1 turns a bare `on:` key into the boolean true. sigs.k8s.io/yaml
	// converts YAML to JSON first, so the key arrives as "true" — rewrite it
	// back to "on" before unmarshalling into the typed shapes.
	fixed, err := normalizeOnKey(data)
	if err != nil {
		return nil, fmt.Errorf("ci: parse yml: %w", err)
	}
	if err := yaml.Unmarshal(fixed, &raw); err != nil {
		return nil, fmt.Errorf("ci: parse yml: %w", err)
	}
	if raw.Version != 0 && raw.Version != 1 {
		return nil, fmt.Errorf("ci: unsupported workflow version %d", raw.Version)
	}

	w := &Workflow{
		Version:     raw.Version,
		Name:        strings.TrimSpace(raw.Name),
		On:          On(raw.On),
		Permissions: raw.Permissions,
		Env:         raw.Env,
		Jobs:        map[string]*WorkflowJob{},
	}
	if w.Name == "" {
		w.Name = "workflow"
	}
	if raw.Concurrency != nil && raw.Concurrency.Group != "" {
		w.Concurrency = &Concurrency{
			Group:            raw.Concurrency.Group,
			CancelInProgress: raw.Concurrency.CancelInProgress,
		}
	}
	// A workflow with no `on:` defaults to the RR events (the v1 behaviour).
	if w.On.RR == nil && w.On.Tag == nil && !w.On.Manual && len(w.On.Schedule) == 0 {
		w.On.RR = &RREvent{}
	}

	ids := make([]string, 0, len(raw.Jobs))
	for id := range raw.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rj := raw.Jobs[id]
		if rj == nil {
			return nil, fmt.Errorf("ci: job %q is empty", id)
		}
		j, err := buildJob(id, rj)
		if err != nil {
			return nil, err
		}
		w.Jobs[id] = j
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// normalizeOnKey rewrites a top-level YAML `on` key that the YAML 1.1 parser
// turned into `true` back into `"on"`. It only touches the document's top level
// (column 0), so a step's `on` string is never affected.
func normalizeOnKey(data []byte) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	inBlock := false // inside a top-level mapping value, where indentation matters
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		indented := ln[0] == ' ' || ln[0] == '\t'
		if !indented {
			inBlock = false
		}
		if indented {
			if !inBlock {
				continue
			}
		}
		trimmed := strings.TrimSpace(ln)
		for _, key := range []string{"on", "true", "True", "TRUE"} {
			if !strings.HasPrefix(trimmed, key+":") {
				continue
			}
			if key != "on" && trimmed != key+":" && !strings.HasPrefix(trimmed, key+": ") {
				continue
			}
			lines[i] = "\"on\":" + strings.TrimPrefix(trimmed, key+":")
			inBlock = true
			break
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func buildJob(id string, rj *rawJob) (*WorkflowJob, error) {
	j := &WorkflowJob{
		ID:              id,
		Name:            strings.TrimSpace(rj.Name),
		Needs:           rj.Needs,
		If:              strings.TrimSpace(rj.If),
		RunsOn:          rj.RunsOn,
		Env:             rj.Env,
		Outputs:         rj.Outputs,
		Required:        rj.Required,
		ContinueOnError: rj.ContinueOnError,
	}
	if j.Name == "" {
		j.Name = id
	}
	if rj.TimeoutMinutes > 0 {
		j.TimeoutMinutes = rj.TimeoutMinutes
	} else if rj.TimeoutSeconds > 0 {
		j.TimeoutMinutes = (rj.TimeoutSeconds + 59) / 60
	}

	if rj.Container != nil {
		j.Container = &Container{
			Image: rj.Container.Image, Env: rj.Container.Env,
			Command: rj.Container.Command, Args: rj.Container.Args,
		}
	}
	if len(rj.Services) > 0 {
		j.Services = map[string]*Service{}
		for name, rs := range rj.Services {
			if rs == nil {
				continue
			}
			j.Services[name] = &Service{
				Image: rs.Image, Env: rs.Env, Ports: rs.Ports,
				Command: rs.Command, Args: rs.Args,
			}
		}
	}
	if rj.Strategy != nil {
		ff := true
		if rj.Strategy.FailFast != nil {
			ff = *rj.Strategy.FailFast
		}
		j.Strategy = &Strategy{
			Matrix:      rj.Strategy.Matrix,
			FailFast:    ff,
			MaxParallel: rj.Strategy.MaxParallel,
		}
		if inc, ok := rj.Strategy.Matrix["include"]; ok {
			j.Strategy.Include = toMapList(inc)
			delete(j.Strategy.Matrix, "include")
		}
		if exc, ok := rj.Strategy.Matrix["exclude"]; ok {
			j.Strategy.Exclude = toMapList(exc)
			delete(j.Strategy.Matrix, "exclude")
		}
	}

	// Steps: `steps:` wins; otherwise the v1 `commands:`/`script:` form becomes
	// one implicit run step (and `image:` becomes the job container).
	for _, rs := range rj.Steps {
		st := Step{
			ID: strings.TrimSpace(rs.ID), Name: strings.TrimSpace(rs.Name),
			If: strings.TrimSpace(rs.If), Uses: strings.TrimSpace(rs.Uses),
			With: rs.With, Run: rs.Run, Shell: rs.Shell, Env: rs.Env,
			WorkingDirectory: rs.WorkingDirectory,
			ContinueOnError:  rs.ContinueOnError,
			TimeoutMinutes:   rs.TimeoutMinutes,
		}
		if st.Run == "" && st.Uses == "" {
			return nil, fmt.Errorf("ci: job %q has a step with neither `run` nor `uses`", id)
		}
		if st.Run != "" && st.Uses != "" {
			return nil, fmt.Errorf("ci: job %q has a step with both `run` and `uses`", id)
		}
		j.Steps = append(j.Steps, st)
	}
	if len(j.Steps) == 0 {
		cmds := rj.Commands
		if len(cmds) == 0 {
			cmds = rj.Script
		}
		if len(cmds) == 0 {
			return nil, fmt.Errorf("ci: job %q has no steps", id)
		}
		j.Steps = []Step{{
			Name: "run", Run: strings.Join(cmds, "\n"),
		}}
		if j.Container == nil && rj.Image != "" {
			j.Container = &Container{Image: rj.Image}
		}
	}
	// The v1 form might carry `image:` WITH steps; honour it as the container.
	if j.Container == nil && rj.Image != "" {
		j.Container = &Container{Image: rj.Image}
	}
	if j.Container != nil && strings.TrimSpace(j.Container.Image) == "" {
		return nil, fmt.Errorf("ci: job %q has a container with no image", id)
	}
	return j, nil
}

// validate rejects unrunnable workflows: dangling `needs`, cycles, and empty
// jobs.
func (w *Workflow) validate() error {
	if len(w.Jobs) == 0 {
		return ErrNoJobs
	}
	for id, j := range w.Jobs {
		for _, need := range j.Needs {
			if _, ok := w.Jobs[need]; !ok {
				return fmt.Errorf("ci: job %q needs unknown job %q", id, need)
			}
			if need == id {
				return fmt.Errorf("ci: job %q needs itself", id)
			}
		}
		if len(j.Steps) == 0 {
			return fmt.Errorf("ci: job %q has no steps", id)
		}
		seen := map[string]bool{}
		for _, st := range j.Steps {
			if st.ID == "" {
				continue
			}
			if seen[st.ID] {
				return fmt.Errorf("ci: job %q has duplicate step id %q", id, st.ID)
			}
			seen[st.ID] = true
		}
	}
	if _, err := w.Order(); err != nil {
		return err
	}
	return nil
}

// Order topologically sorts the JOBS into execution stages (matrix expansion
// happens later, in Plan). A cycle is an error.
func (w *Workflow) Order() ([][]string, error) {
	indeg := make(map[string]int, len(w.Jobs))
	next := make(map[string][]string, len(w.Jobs))
	for id, j := range w.Jobs {
		indeg[id] = len(j.Needs)
		for _, n := range j.Needs {
			next[n] = append(next[n], id)
		}
	}
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var stages [][]string
	seen := 0
	for len(ready) > 0 {
		stage := append([]string(nil), ready...)
		stages = append(stages, stage)
		seen += len(stage)
		var nxt []string
		for _, id := range stage {
			for _, d := range next[id] {
				indeg[d]--
				if indeg[d] == 0 {
					nxt = append(nxt, d)
				}
			}
		}
		sort.Strings(nxt)
		ready = nxt
	}
	if seen != len(w.Jobs) {
		return nil, fmt.Errorf("ci: workflow DAG has a cycle")
	}
	return stages, nil
}

// Plan expands the workflow's matrix into executable instances and groups them
// into stages. It is the unit the orchestrator schedules.
func (w *Workflow) Plan() (*Plan, error) {
	stages, err := w.Order()
	if err != nil {
		return nil, err
	}
	// keysOf maps a job id to every instance key it expands to, so `needs` can
	// be rewritten to instance keys.
	keysOf := map[string][]string{}
	for _, id := range w.jobOrder() {
		keysOf[id] = instanceKeys(w.Jobs[id])
	}
	p := &Plan{Workflow: w}
	for _, stage := range stages {
		insts := make([]*Instance, 0, len(stage))
		for _, id := range stage {
			j := w.Jobs[id]
			dims := matrixKeys(j.Strategy)
			for _, m := range expandMatrix(j.Strategy) {
				inst := &Instance{Key: instanceKeyDims(id, m, dims), Job: j, Matrix: m}
				for _, need := range j.Needs {
					inst.Needs = append(inst.Needs, keysOf[need]...)
				}
				insts = append(insts, inst)
			}
			if j.Required {
				p.Required = true
			}
		}
		sort.Slice(insts, func(a, b int) bool { return insts[a].Key < insts[b].Key })
		p.Stages = append(p.Stages, insts)
	}
	return p, nil
}

// expandMatrix returns one value map per combination (a single empty map when
// there is no matrix). `exclude` removes matches; `include` adds or augments.
func expandMatrix(s *Strategy) []map[string]any {
	if s == nil || len(s.Matrix) == 0 {
		if s != nil && len(s.Include) > 0 {
			out := make([]map[string]any, 0, len(s.Include))
			for _, inc := range s.Include {
				out = append(out, copyMap(inc))
			}
			return out
		}
		return []map[string]any{nil}
	}
	keys := make([]string, 0, len(s.Matrix))
	for k := range s.Matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	combos := []map[string]any{{}}
	for _, k := range keys {
		vals := asList(s.Matrix[k])
		next := make([]map[string]any, 0, len(combos)*len(vals))
		for _, c := range combos {
			for _, v := range vals {
				m := copyMap(c)
				m[k] = v
				next = append(next, m)
			}
		}
		combos = next
	}

	// exclude removes matching combinations (every listed key must equal).
	if len(s.Exclude) > 0 {
		kept := combos[:0]
		for _, c := range combos {
			if !matchesAny(s.Exclude, c) {
				kept = append(kept, c)
			}
		}
		combos = kept
	}

	// include: augment an existing combination when every key it shares with the
	// matrix matches (extra include keys are NEW dimensions, added to the
	// combination), otherwise append a new one — GitHub's semantics.
	for _, inc := range s.Include {
		merged := false
		for _, c := range combos {
			if includeMatches(inc, c, keys) {
				for k, v := range inc {
					c[k] = v
				}
				merged = true
				break
			}
		}
		if !merged {
			combos = append(combos, copyMap(inc))
		}
	}
	return combos
}

// includeMatches reports whether an `include:` entry augments combination c:
// every key the include shares with the MATRIX must already have that value.
// Keys the matrix does not declare are new dimensions and always apply.
func includeMatches(inc, c map[string]any, matrixKeys []string) bool {
	inMatrix := make(map[string]bool, len(matrixKeys))
	for _, k := range matrixKeys {
		inMatrix[k] = true
	}
	for k, v := range inc {
		if !inMatrix[k] {
			continue
		}
		cv, ok := c[k]
		if !ok || fmt.Sprint(cv) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

func matchesAny(filters []map[string]any, c map[string]any) bool {
	for _, f := range filters {
		if matchesSubset(f, c) {
			return true
		}
	}
	return false
}

func matchesSubset(f, c map[string]any) bool {
	for k, v := range f {
		cv, ok := c[k]
		if !ok || fmt.Sprint(cv) != fmt.Sprint(v) {
			return false
		}
	}
	return true
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func instanceKey(jobID string, m map[string]any) string {
	return instanceKeyDims(jobID, m, nil)
}

// instanceKeyDims builds an instance key from the MATRIX dimensions only: an
// `include:` entry that adds a new key (e.g. `experimental: true`) augments an
// existing instance instead of renaming it. When the matrix declares no
// dimension (an include-only job), every key of the value map is used.
func instanceKeyDims(jobID string, m map[string]any, matrixKeys []string) string {
	if len(m) == 0 {
		return jobID
	}
	keys := matrixKeys
	if len(keys) == 0 {
		keys = make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	} else {
		// Only keys this combination actually has (an excluded dimension can
		// leave a sparse include-only entry).
		kept := make([]string, 0, len(keys))
		for _, k := range keys {
			if _, ok := m[k]; ok {
				kept = append(kept, k)
			}
		}
		keys = kept
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, instanceKeyPart(k, m[k]))
	}
	if len(parts) == 0 {
		return jobID
	}
	return jobID + "-" + strings.Join(parts, "-")
}

// instanceKeyPart is the matrix-value part of a key: an integer-valued string
// keeps its digits, and dots are preserved so `go: 1.27` reads as "go-1.27".
func instanceKeyPart(k string, v any) string {
	s := fmt.Sprint(v)
	if n, err := strconv.ParseFloat(s, 64); err == nil && n == float64(int64(n)) {
		s = strconv.FormatInt(int64(n), 10)
	}
	return nameFragment(k + "-" + s)
}

// nameFragment lowercases and replaces anything outside [a-z0-9.-] with '-'.
// Unlike sanitize it KEEPS dots, because an instance key is a display/index
// value, not a Kubernetes object name.
func nameFragment(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "x"
	}
	return out
}

// instanceKeys returns every key a job expands to.
func instanceKeys(j *WorkflowJob) []string {
	dims := matrixKeys(j.Strategy)
	combos := expandMatrix(j.Strategy)
	out := make([]string, 0, len(combos))
	for _, m := range combos {
		out = append(out, instanceKeyDims(j.ID, m, dims))
	}
	return out
}

// matrixKeys returns the sorted dimension names a matrix declares (excluding
// the include/exclude pseudo-keys).
func matrixKeys(s *Strategy) []string {
	if s == nil || len(s.Matrix) == 0 {
		return nil
	}
	keys := make([]string, 0, len(s.Matrix))
	for k := range s.Matrix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toStrings(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return []string{fmt.Sprint(v)}
}

// asList normalises a matrix value into a list of scalars, keeping the original
// types so `go: [1.27]` stays a number while `go: ["1.27"]` stays a string.
func asList(v any) []any {
	switch x := v.(type) {
	case nil:
		return []any{nil}
	case []any:
		return x
	}
	return []any{v}
}

func toMapList(v any) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		if m, ok := v.(map[string]any); ok {
			return []map[string]any{m}
		}
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
