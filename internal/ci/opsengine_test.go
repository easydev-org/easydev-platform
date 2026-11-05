package ci

import (
	"strings"
	"testing"
)

// TestMaterialiseIncludesCheckout is the regression test for the original bug:
// the orchestrator computed a Checkout and the executor DROPPED it, so every CI
// job ran in an empty working directory.
func TestMaterialiseIncludesCheckout(t *testing.T) {
	o := &EngineOps{VCSBase: "http://vcs.local", VCSUser: "admin", VCSToken: "secret"}
	script, env := o.materialise(JobSpec{
		Namespace: "team-a",
		Commands:  []string{"go test ./..."},
		Checkout:  &CheckoutSpec{Ref: "abc123", Path: "", Dest: "."},
	})

	if !strings.Contains(script, "http://vcs.local/api/v1/namespaces/team-a/archive/abc123.tar.gz") {
		t.Fatalf("checkout URL missing from the script:\n%s", script)
	}
	if !strings.Contains(script, "tar -xz") {
		t.Fatalf("checkout must untar:\n%s", script)
	}
	if !strings.Contains(script, "go test ./...") {
		t.Fatalf("the step's command must follow the checkout:\n%s", script)
	}
	if env["EASYDEV_CHECKOUT_REF"] != "abc123" {
		t.Fatalf("checkout ref not exported: %v", env)
	}
	// The credential is passed via ENV, never interpolated into the script, so a
	// token with shell metacharacters cannot break out.
	if strings.Contains(script, "secret") {
		t.Fatal("the token must not appear in the script")
	}
	if !strings.Contains(script, `-u "$EASYDEV_CHECKOUT_AUTH"`) {
		t.Fatalf("expected an env-based credential:\n%s", script)
	}
}

// TestMaterialiseSubPathCheckout: a sub-scope checkout uses ?path=.
func TestMaterialiseSubPathCheckout(t *testing.T) {
	o := &EngineOps{VCSBase: "http://vcs.local/"}
	script, _ := o.materialise(JobSpec{
		Namespace: "team-a",
		Commands:  []string{"make"},
		Checkout:  &CheckoutSpec{Ref: "sha", Path: "services/api"},
	})
	if !strings.Contains(script, "?path=services/api") {
		t.Fatalf("sub-path checkout missing:\n%s", script)
	}
}

// TestMaterialiseCheckoutPrecedesWorkdir pins the ordering: the working
// directory is usually a subdirectory OF the checkout, so the tree must land
// first.
func TestMaterialiseCheckoutPrecedesWorkdir(t *testing.T) {
	o := &EngineOps{VCSBase: "http://vcs.local"}
	script, _ := o.materialise(JobSpec{
		Namespace: "team-a",
		Commands:  []string{"make"},
		Checkout:  &CheckoutSpec{Ref: "sha"},
		WorkDir:   "services/api",
	})
	ci := strings.Index(script, "tar -xz")
	wd := strings.Index(script, "services/api")
	if ci < 0 || wd < 0 {
		t.Fatalf("script missing checkout or workdir:\n%s", script)
	}
	if ci > wd {
		t.Fatalf("the checkout must happen BEFORE cd into the workdir:\n%s", script)
	}
}

// TestMaterialiseNoCheckout: a job without a checkout must not fetch anything
// (e.g. a step that only calls an API).
func TestMaterialiseNoCheckout(t *testing.T) {
	o := &EngineOps{VCSBase: "http://vcs.local"}
	script, _ := o.materialise(JobSpec{Namespace: "team-a", Commands: []string{"echo hi"}})
	if strings.Contains(script, "curl") {
		t.Fatalf("no checkout must mean no fetch:\n%s", script)
	}
	if !strings.Contains(script, "echo hi") {
		t.Fatalf("command missing:\n%s", script)
	}
}

// TestMaterialiseEmitsOutputs: the script must print the step output file behind
// the marker the orchestrator parses.
func TestMaterialiseEmitsOutputs(t *testing.T) {
	o := &EngineOps{}
	script, env := o.materialise(JobSpec{Commands: []string{"echo x=1 > $EASYDEV_OUTPUT"}})
	if env["EASYDEV_OUTPUT"] == "" {
		t.Fatal("EASYDEV_OUTPUT must be exported")
	}
	if !strings.Contains(script, "##[easydev:outputs]") {
		t.Fatalf("output marker missing:\n%s", script)
	}
}

func TestParseOutputs(t *testing.T) {
	logs := "hello\n##[easydev:outputs]\nversion=1.2.3\nsha=abc\n"
	got := parseOutputs("job", logs)
	if got["version"] != "1.2.3" || got["sha"] != "abc" {
		t.Fatalf("outputs = %v", got)
	}
	if parseOutputs("job", "no marker") != nil {
		t.Fatal("no marker means no outputs")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":        "''",
		"abc":     "'abc'",
		"a'b":     `'a'\''b'`,
		"a b; rm": "'a b; rm'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
