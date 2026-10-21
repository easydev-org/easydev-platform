package ci

import (
	"fmt"
	"sort"

	"sigs.k8s.io/yaml"
)

// rawPipeline mirrors the on-disk `.easydev-ci.yml` shape. It is intentionally
// close to GitHub Actions so a team can migrate a workflow with small edits.
//
//	version: 1
//	on: [rr_opened, rr_synced, rr_merged]
//	jobs:
//	  test:
//	    image: golang:1.27
//	    commands: ["go vet ./...", "go test ./..."]
//	    required: true
//	  build:
//	    needs: [test]
//	    image: golang:1.27
//	    commands: ["go build ./..."]
//	    artifacts: ["bin/*"]
type rawPipeline struct {
	Version int               `json:"version"`
	On      []string          `json:"on"`
	Jobs    map[string]rawJob `json:"jobs"`
}

type rawJob struct {
	Needs     []string          `json:"needs"`
	Image     string            `json:"image"`
	Commands  []string          `json:"commands"`
	Script    []string          `json:"script"` // alias for commands
	Env       map[string]string `json:"env"`
	CPU       string            `json:"cpu"`
	Memory    string            `json:"memory"`
	Timeout   int               `json:"timeout_seconds"`
	Artifacts []string          `json:"artifacts"`
	Required  bool              `json:"required"`
}

// ParseYAML parses and validates a `.easydev-ci.yml`.
func ParseYAML(data []byte) (*Pipeline, error) {
	var raw rawPipeline
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("ci: parse yml: %w", err)
	}
	if raw.Version != 0 && raw.Version != 1 {
		return nil, fmt.Errorf("ci: unsupported pipeline version %d", raw.Version)
	}
	p := &Pipeline{Name: "pipeline", Jobs: map[string]*Job{}}
	// `on:` may be omitted => default to RR events.
	if len(raw.On) == 0 {
		raw.On = []string{"rr_opened", "rr_synced"}
	}
	for _, s := range raw.On {
		t, ok := ParseTrigger(s)
		if !ok {
			return nil, fmt.Errorf("ci: unknown trigger %q", s)
		}
		p.On = append(p.On, t)
	}
	ids := make([]string, 0, len(raw.Jobs))
	for id := range raw.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rj := raw.Jobs[id]
		cmds := rj.Commands
		if len(cmds) == 0 {
			cmds = rj.Script
		}
		if rj.Image == "" {
			return nil, fmt.Errorf("ci: job %q has no image", id)
		}
		if len(cmds) == 0 {
			return nil, fmt.Errorf("ci: job %q has no commands", id)
		}
		p.Jobs[id] = &Job{
			ID: id, Needs: rj.Needs, Image: rj.Image, Commands: cmds,
			Env: rj.Env, CPU: rj.CPU, Memory: rj.Memory,
			TimeoutSeconds: rj.Timeout, Artifacts: rj.Artifacts, Required: rj.Required,
		}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}
