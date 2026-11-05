// Package ci turns a repository's workflow file into a DAG of jobs, runs them
// on an execution backend (Kubernetes Jobs via easyops, or an in-process fake),
// and reports the result back to easyvcs as CHECKS.
//
// A workflow is `.easydev/workflows/<name>.yml` in a namespace. Its coordinate
// is (namespace, path, snapshot_sha) — never a branch — so a run is byte-for-byte
// reproducible: the file is read OUT of the pinned snapshot, and `workflow_sha`
// records exactly which version ran.
package ci

import (
	"fmt"
	"sort"
	"strings"
)

// Trigger names the event that starts a pipeline.
type Trigger string

const (
	TriggerRROpened Trigger = "rr_opened"
	TriggerRRSynced Trigger = "rr_synced"
	TriggerRRMerged Trigger = "rr_merged"
	TriggerTag      Trigger = "tag"
	TriggerManual   Trigger = "manual"
	TriggerSchedule Trigger = "schedule"
)

// EventType is the raw event kind reported by easyvcs (before it is narrowed to
// a Trigger by the workflow's `on:` block).
type EventType string

const (
	EventRROpened EventType = "rr_opened"
	EventRRSynced EventType = "rr_synced"
	EventRRMerged EventType = "rr_merged"
	EventTag      EventType = "tag_created"
	EventManual   EventType = "manual"
	EventSchedule EventType = "schedule"
)

// Workflow is a parsed workflow file. It is a superset of the v1 Pipeline: a
// v1 file (jobs with `commands:`) parses into a workflow whose jobs each have a
// single implicit `run` step.
type Workflow struct {
	Version     int
	Name        string
	On          On
	Permissions map[string]string
	Concurrency *Concurrency
	Env         map[string]string
	Jobs        map[string]*WorkflowJob
	// Source is the file the workflow came from (repopath), for diagnostics.
	Source string
}

// On declares which events start the workflow, plus their filters.
type On struct {
	// RR is set when Revision-Request events are subscribed.
	RR *RREvent
	// Tag is set when tag creation is subscribed.
	Tag *TagEvent
	// Manual enables workflow_dispatch.
	Manual bool
	// Schedule is a cron expression ("" = no schedule).
	Schedule []string
}

// RREvent narrows the RR triggers.
type RREvent struct {
	// Types is the subset of opened/synced/merged to react to. Empty = all.
	Types []string
	// Paths are glob filters applied to the change's paths. Empty = all.
	Paths []string
	// Branches are glob filters applied to the TARGET branch. Empty = all.
	Branches []string
	// Base is the branch whose workflow file is authoritative. Empty = the RR's
	// target branch. A workflow from the head snapshot is NEVER used unless
	// AllowHead is set (that is what makes "modify CI to bypass CI" impossible).
	Base string
	// AllowHead permits reading the workflow from the head snapshot. It exists
	// for trusted, non-RR triggers (manual/schedule) and is deliberately not
	// reachable from `on.rr` in the public API.
	AllowHead bool
}

// TagEvent narrows the tag trigger.
type TagEvent struct {
	Pattern string
}

// Concurrency serialises runs that share a group; a newer run cancels the older
// one when CancelInProgress is set.
type Concurrency struct {
	Group            string
	CancelInProgress bool
}

// WorkflowJob is one job before matrix expansion.
type WorkflowJob struct {
	ID   string
	Name string

	// Needs lists job ids that must SUCCEED before this one runs.
	Needs []string
	// If is an expression evaluated before the job runs (empty = always).
	If string

	// RunsOn is a label set the executor must satisfy (e.g. [linux, amd64]).
	RunsOn []string

	// Strategy expands this job into one instance per matrix combination.
	Strategy *Strategy

	// Container is the image (and env) every step runs in unless a step
	// overrides it.
	Container *Container
	// Services are long-lived companions started with the job.
	Services map[string]*Service

	Env   map[string]string
	Steps []Step

	// Outputs maps an output name to an expression over steps.*.outputs.
	Outputs map[string]string

	// Artifacts are glob paths (relative to /workspace) uploaded to the package
	// registry after the job succeeds, so a later job or a human can fetch them.
	// A pattern ending in "/" is archived as a directory.
	Artifacts []string

	// Required marks a job whose failure BLOCKS a merge (a required check).
	Required bool
	// TimeoutMinutes bounds the whole job (0 = no deadline).
	TimeoutMinutes int
	// ContinueOnError makes a failure non-fatal for the run.
	ContinueOnError bool
}

// Strategy is the matrix expansion policy.
type Strategy struct {
	Matrix      map[string]any
	Include     []map[string]any
	Exclude     []map[string]any
	FailFast    bool
	MaxParallel int
}

// Container is a job/step execution image.
type Container struct {
	Image   string
	Env     map[string]string
	Command []string
	Args    []string
}

// Service is a companion container (a database, a cache) reachable by its map
// key as a hostname.
type Service struct {
	Image   string
	Env     map[string]string
	Ports   []int
	Command []string
	Args    []string
}

// Step is one executable unit inside a job.
type Step struct {
	// ID names the step for `steps.<id>.outputs`; optional except for a step
	// that produces outputs.
	ID   string
	Name string
	If   string

	// Uses is an action reference `namespace/path@ref`; exclusive with Run.
	Uses string
	// With is the action's input map.
	With map[string]string
	// Run is a shell script; exclusive with Uses.
	Run string
	// Shell selects the interpreter ("bash", "sh", "pwsh"); default bash.
	Shell string

	Env              map[string]string
	WorkingDirectory string
	ContinueOnError  bool
	TimeoutMinutes   int
}

// Plan is a workflow expanded into executable job instances.
type Plan struct {
	Workflow *Workflow
	// Stages groups job instances that may run in parallel. Every stage only
	// starts after the previous one succeeded.
	Stages [][]*Instance
	// Required reports whether any instance is required (a merge-blocking
	// check exists).
	Required bool
}

// Instance is one (job, matrix combination) pair: the unit the executor runs.
type Instance struct {
	// Key is the job id plus a matrix suffix, e.g. "test" or "test-go1.27-linux".
	Key string
	// Job is the unexpanded definition.
	Job *WorkflowJob
	// Matrix is this instance's matrix values (nil for a matrix-less job).
	Matrix map[string]any
	// Needs lists the KEYS of instances that must succeed first.
	Needs []string
}

// TriggeredBy reports whether the workflow subscribes to the event.
func (w *Workflow) TriggeredBy(ev *Event) bool {
	switch ev.Event {
	case EventRROpened, EventRRSynced, EventRRMerged:
		if w.On.RR == nil {
			return false
		}
		want := rrTypeName(ev.Event)
		if len(w.On.RR.Types) > 0 && !containsFold(w.On.RR.Types, want) {
			return false
		}
		if len(w.On.RR.Paths) > 0 && !anyPathMatch(w.On.RR.Paths, ev.ChangedPaths) {
			return false
		}
		if len(w.On.RR.Branches) > 0 && !anyGlobMatch(w.On.RR.Branches, ev.Ref) {
			return false
		}
		return true

	case EventTag:
		if w.On.Tag == nil {
			return false
		}
		if w.On.Tag.Pattern != "" && !globMatch(w.On.Tag.Pattern, ev.Tag) {
			return false
		}
		return true

	case EventManual:
		return w.On.Manual
	case EventSchedule:
		return len(w.On.Schedule) > 0
	}
	return false
}

func rrTypeName(t EventType) string {
	switch t {
	case EventRROpened:
		return "opened"
	case EventRRSynced:
		return "synced"
	case EventRRMerged:
		return "merged"
	}
	return string(t)
}

// RequiredJobs returns the job ids marked required.
func (w *Workflow) RequiredJobs() []string {
	out := make([]string, 0)
	for id, j := range w.Jobs {
		if j.Required {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// jobOrder is a deterministic job-id order (map iteration is random).
func (w *Workflow) jobOrder() []string {
	ids := make([]string, 0, len(w.Jobs))
	for id := range w.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func containsFold(list []string, s string) bool {
	for _, e := range list {
		if strings.EqualFold(e, s) {
			return true
		}
	}
	return false
}

// anyPathMatch reports whether any changed path matches any pattern.
func anyPathMatch(patterns, paths []string) bool {
	for _, p := range paths {
		if anyGlobMatch(patterns, p) {
			return true
		}
	}
	return false
}

func anyGlobMatch(patterns []string, s string) bool {
	for _, p := range patterns {
		if globMatch(p, s) {
			return true
		}
	}
	return false
}

// globMatch is a small glob: `*` matches within a segment, `**` crosses
// segments, `?` matches one character, and a bare prefix matches a segment
// prefix (so "src/**" covers "src/a/b.go").
func globMatch(pattern, s string) bool {
	return globSegments(strings.Split(pattern, "/"), strings.Split(s, "/"))
}

func globSegments(pat, seg []string) bool {
	if len(pat) == 0 {
		return len(seg) == 0
	}
	if pat[0] == "**" {
		// ** matches zero or more segments.
		for i := 0; i <= len(seg); i++ {
			if globSegments(pat[1:], seg[i:]) {
				return true
			}
		}
		return false
	}
	if len(seg) == 0 {
		return false
	}
	if !segmentMatch(pat[0], seg[0]) {
		return false
	}
	return globSegments(pat[1:], seg[1:])
}

func segmentMatch(pat, s string) bool {
	// Iterative wildcard match with backtracking (no regexp: no ReDoS surface).
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(pat) && (pat[pi] == '?' || pat[pi] == s[si]):
			pi++
			si++
		case pi < len(pat) && pat[pi] == '*':
			star = pi
			mark = si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}

// sanitize turns an arbitrary string into a DNS-1123-ish k8s name fragment.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "run"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// ErrNoJobs is returned when a workflow declares no jobs.
var ErrNoJobs = fmt.Errorf("ci: workflow has no jobs")
