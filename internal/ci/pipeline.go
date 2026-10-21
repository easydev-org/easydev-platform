// Package ci is the orchestration layer that turns a repository's
// `.easydev-ci.yml` into a DAG of jobs, runs them on easyops (Kubernetes Jobs /
// BuildKit), and reports the result back to easyvcs as CHECKS.
//
// This package is the ONE thing none of the three core servers provides:
//   - easyvcs knows revisions, drafts and Revision Requests, but has no notion
//     of "run something when an RR opens".
//   - easyops can run a Job / build an image, but has no notion of "which jobs
//     to run and in what order".
//   - easyblobs stores and serves packages, but does not schedule anything.
//
// The orchestrator is therefore deliberately small and stateless where it can
// be: all durable state (pipelines, runs, checks, secrets) lives behind the
// Store interface, so the orchestrator itself is a pure function of
// (event, store, opsclient).
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
)

// Pipeline is a parsed `.easydev-ci.yml`.
type Pipeline struct {
	Name string
	// On lists the triggers this pipeline reacts to.
	On []Trigger
	// Jobs is the DAG, keyed by job id.
	Jobs map[string]*Job
}

// Job is one executable unit. It maps onto exactly one easyops Job (a
// batch/v1.Job running a container to completion).
type Job struct {
	// ID is the key in the jobs map.
	ID string
	// Needs lists job ids that must SUCCEED before this one runs (the DAG).
	Needs []string
	// Image is the container image (e.g. golang:1.27, node:22, or a repo image
	// built by an earlier job).
	Image string
	// Commands is the shell script run in the container.
	Commands []string
	// Env is the extra environment for the container.
	Env map[string]string
	// CPU / Memory are resource requests ("500m", "1Gi").
	CPU    string
	Memory string
	// TimeoutSeconds bounds the job (0 = no deadline).
	TimeoutSeconds int
	// Artifacts are glob paths to upload to easyblobs after a successful run.
	Artifacts []string
	// Required marks a job whose failure BLOCKS a merge (a required check).
	Required bool
}

// Validate rejects an empty pipeline, unknown triggers, and dangling `needs`
// edges (which would make the DAG unrunnable).
func (p *Pipeline) Validate() error {
	if len(p.Jobs) == 0 {
		return fmt.Errorf("ci: pipeline has no jobs")
	}
	if len(p.On) == 0 {
		return fmt.Errorf("ci: pipeline has no `on:` triggers")
	}
	for _, t := range p.On {
		switch t {
		case TriggerRROpened, TriggerRRSynced, TriggerRRMerged, TriggerTag, TriggerManual:
		default:
			return fmt.Errorf("ci: unknown trigger %q", t)
		}
	}
	for id, j := range p.Jobs {
		for _, need := range j.Needs {
			if _, ok := p.Jobs[need]; !ok {
				return fmt.Errorf("ci: job %q needs unknown job %q", id, need)
			}
			if need == id {
				return fmt.Errorf("ci: job %q needs itself", id)
			}
		}
	}
	if _, err := p.Order(); err != nil {
		return err
	}
	return nil
}

// Order topologically sorts the jobs into EXECUTION STAGES: every job in a
// stage may run in parallel, and every stage only starts when the previous one
// has succeeded. A cycle is an error.
func (p *Pipeline) Order() ([][]string, error) {
	indeg := make(map[string]int, len(p.Jobs))
	next := make(map[string][]string, len(p.Jobs))
	for id, j := range p.Jobs {
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
	sort.Strings(ready) // deterministic stage order
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
	if seen != len(p.Jobs) {
		return nil, fmt.Errorf("ci: pipeline DAG has a cycle")
	}
	return stages, nil
}

// TriggeredBy reports whether the pipeline should run for the given trigger.
func (p *Pipeline) TriggeredBy(t Trigger) bool {
	for _, on := range p.On {
		if on == t {
			return true
		}
	}
	return false
}

// ParseTrigger turns a raw event name into a Trigger, tolerating the spellings
// easyvcs events use.
func ParseTrigger(s string) (Trigger, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "rr_opened", "rr.opened", "opened":
		return TriggerRROpened, true
	case "rr_synced", "rr.synced", "synced", "updated":
		return TriggerRRSynced, true
	case "rr_merged", "rr.merged", "merged":
		return TriggerRRMerged, true
	case "tag", "tag_created", "tag.created":
		return TriggerTag, true
	case "manual", "workflow_dispatch":
		return TriggerManual, true
	}
	return "", false
}
