package ci

import (
	"os"
	"testing"
)

// TestRepoWorkflowParses keeps the repo's OWN workflow file valid: the platform
// must be able to parse the file it ships, or a schema change silently breaks
// the example everyone copies.
func TestRepoWorkflowParses(t *testing.T) {
	data, err := os.ReadFile("../../.easydev/workflows/ci.yml")
	if err != nil {
		t.Skipf("the repo workflow is not present: %v", err)
	}
	w, err := ParseWorkflow(data)
	if err != nil {
		t.Fatalf("the repo's .easydev/workflows/ci.yml does not parse: %v", err)
	}
	if len(w.Jobs) == 0 {
		t.Fatal("the repo workflow has no jobs")
	}
	plan, err := w.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Required {
		t.Error("the repo workflow should mark a job required so the merge gate applies")
	}
	// Every `uses:` must parse as an action reference.
	for id, j := range w.Jobs {
		for _, st := range j.Steps {
			if st.Uses == "" {
				continue
			}
			if _, err := ParseActionRef(st.Uses); err != nil {
				t.Errorf("job %s: %v", id, err)
			}
		}
	}
}

// TestDocsWorkflowParses parses the example in docs/ci.md's fenced block, so the
// documentation cannot drift from the parser.
func TestDocsWorkflowExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../docs/ci.md")
	if err != nil {
		t.Skipf("docs/ci.md is not present: %v", err)
	}
	example := extractYAMLBlock(string(data))
	if example == "" {
		t.Skip("no workflow example found in docs/ci.md")
	}
	if _, err := ParseWorkflow([]byte(example)); err != nil {
		t.Fatalf("the workflow example in docs/ci.md does not parse: %v\n---\n%s", err, example)
	}
}

// extractYAMLBlock returns the first ```yaml block that declares a workflow.
func extractYAMLBlock(doc string) string {
	const fence = "```yaml"
	for {
		i := indexFrom(doc, fence, 0)
		if i < 0 {
			return ""
		}
		start := i + len(fence)
		if j := indexFrom(doc[start:], "\n", 0); j >= 0 {
			start += j + 1
		}
		end := indexFrom(doc[start:], "```", 0)
		if end < 0 {
			return ""
		}
		block := doc[start : start+end]
		if indexFrom(block, "jobs:", 0) >= 0 && indexFrom(block, "on:", 0) >= 0 {
			return block
		}
		doc = doc[start+end+3:]
	}
}

func indexFrom(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	i := indexOf(s[from:], sub)
	if i < 0 {
		return -1
	}
	return from + i
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
