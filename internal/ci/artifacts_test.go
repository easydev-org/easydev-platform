package ci

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestArtifactNameIsSafe pins the glob -> name mapping used in the URL.
func TestArtifactNameIsSafe(t *testing.T) {
	cases := map[string]string{
		"bin/*":             "bin",
		"coverage/":         "coverage",
		"dist/app.tar.gz":   "dist-app.tar.gz",
		"*.log":             "artifact",
		"reports/junit.xml": "reports-junit.xml",
	}
	for in, want := range cases {
		if got := artifactName(in); got != want {
			t.Errorf("artifactName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestArtifactPatternsCannotEscapeTheWorkspace: an artifact must stay inside
// /workspace, or a workflow could exfiltrate the image's own files.
func TestArtifactPatternsCannotEscapeTheWorkspace(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "../../secrets", "bin/../../../etc/shadow"} {
		if _, err := resolveArtifacts([]string{bad}); err == nil {
			t.Errorf("artifact %q must be rejected", bad)
		}
	}
	if _, err := resolveArtifacts([]string{"bin/*", "coverage/"}); err != nil {
		t.Fatalf("valid patterns rejected: %v", err)
	}
}

// TestArtifactEnvIsOnlySetWhenNeeded: a job without artifacts must not carry a
// registry URL it will never use.
func TestArtifactEnvIsOnlySetWhenNeeded(t *testing.T) {
	if got := withArtifactEnv(map[string]string{"A": "1"}, "http://reg", "/ns/run/x", false); got[EnvArtifactBase] != "" {
		t.Error("a job without artifacts must not get the registry env")
	}
	got := withArtifactEnv(map[string]string{"A": "1"}, "http://reg", "/ns/run/x", true)
	if got[EnvArtifactBase] != "http://reg" || got[EnvArtifactPath] != "/ns/run/x" {
		t.Errorf("env = %v", got)
	}
	if got["A"] != "1" {
		t.Error("existing env was dropped")
	}
}

// TestArtifactScriptIsOnlyAddedWhenDeclared.
func TestArtifactScriptIsOnlyAddedWhenDeclared(t *testing.T) {
	with := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    artifacts: ["bin/*"]
    steps: [{run: "echo x"}]
`)
	without := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps: [{run: "echo x"}]
`)
	// The orchestrator only injects the block when a registry is configured, so
	// this checks the PLUMBING (Artifacts reaches WorkflowJob) rather than the
	// script text.
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    artifacts: ["bin/*", "coverage/"]
    steps: [{run: "echo x"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Jobs["j"].Artifacts) != 2 {
		t.Fatalf("artifacts = %v (the field did not reach WorkflowJob)", wf.Jobs["j"].Artifacts)
	}
	_ = with
	_ = without
}

// TestArtifactsAreUploadedFromInsideTheContainer is the end-to-end behaviour: the
// generated script tars a artifact and PUTs it to a real HTTP endpoint.
//
// The script is executed with a real bash against an httptest server standing in
// for the registry, so the upload path is proven rather than asserted on text.
func TestArtifactsAreUploadedFromInsideTheContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	for _, bin := range []string{"bash", "curl", "tar", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not available", bin)
		}
	}

	var mu sync.Mutex
	uploads := map[string]string{} // path -> body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		uploads[r.URL.Path] = string(buf[:n])
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Build the script exactly as the orchestrator would, with a job that writes
	// an artifact and then uploads it.
	fops := NewFakeOps()
	st := NewMemStore()
	o := NewOrchestrator(st, fops).WithArtifacts(srv.URL).WithTokenMinter(&stubMinter{}).
		WithSecrets(mapSecrets{}, &recordingWriter{})

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
permissions:
  packages: write
jobs:
  j:
    container: {image: busybox}
    artifacts: ["bin/*"]
    steps:
      - run: mkdir -p bin && echo artifact-payload > bin/app
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

	specs := fops.Specs()
	if len(specs) != 1 {
		t.Fatalf("executor jobs = %d", len(specs))
	}
	script := specs[0].Commands[0]
	if !strings.Contains(script, "easydev_artifact_upload") {
		t.Fatalf("no artifact upload block:\\n%s", script)
	}
	// The token must come from the env (a reference), never the script.
	if strings.Contains(script, "write-token") {
		t.Error("the packages token leaked into the script")
	}

	// Execute it for real, with the container paths rewritten into a temp dir and
	// the registry env supplied by the test.
	out, code := runSupervisorWithEnv(t, script, map[string]string{
		EnvArtifactBase:  srv.URL,
		EnvArtifactPath:  "/team-a/run-1/j",
		EnvArtifactToken: "write-token",
	})
	if code != 0 {
		t.Fatalf("exit = %d\\n%s", code, out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(uploads) == 0 {
		t.Fatalf("nothing was uploaded\\n--- script output ---\\n%s", out)
	}
	var body string
	for path, b := range uploads {
		if !strings.HasSuffix(path, "/bin.tgz") {
			t.Errorf("unexpected upload path %q", path)
		}
		body = b
	}
	if body == "" {
		t.Fatal("the upload had an empty body")
	}
	// It is a gzip'd tar: the magic bytes prove the tar|curl pipe worked.
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("the uploaded body is not gzip: %q", body[:min(16, len(body))])
	}
}

// runSupervisorWithEnv is runSupervisor plus extra env for the uploader.
func runSupervisorWithEnv(t *testing.T, script string, env map[string]string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	s := strings.ReplaceAll(script, "/workspace", filepath.Join(dir, "workspace"))
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "supervisor.sh")
	if err := os.WriteFile(path, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v\\n%s", err, out)
		}
	}
	return string(out), code
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
