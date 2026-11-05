package ci

import (
	"fmt"
	"strings"
)

// Artifacts.
//
// A job may declare glob paths to keep after it succeeds:
//
//	jobs:
//	  build:
//	    artifacts: ["bin/*", "coverage/"]
//
// The upload happens INSIDE the job's container, at the END of the supervisor
// script, by `tar | curl -T` to the package registry's `generic` protocol. That
// means:
//
//   - no new RPC on easyops (an artifact is a package, and the registry already
//     stores packages);
//   - no file bytes ever cross the orchestrator: a big binary does not travel
//     through the control plane or the store;
//   - the upload uses the SAME scoped credential the job already has, so a
//     `permissions: packages: write` is what actually authorises it.
//
// The orchestrator records the resulting URL, so a later job (or a human) can
// find the artifact from the run record.

// ArtifactURLPrefix is the path an artifact is published under.
//
// The layout is `<prefix>/<namespace>/<run>/<instance>/<name>` so one tenant's
// artifacts cannot collide with another's, and a run's artifacts are grouped.
const ArtifactURLPrefix = "/artifacts/generic"

// artifactEnv carries what the in-container uploader needs.
const (
	EnvArtifactBase  = "EASYDEV_ARTIFACTS_URL"  // the registry base URL
	EnvArtifactPath  = "EASYDEV_ARTIFACTS_PATH" // "<namespace>/<run>/<instance>"
	EnvArtifactToken = "EASYDEV_PACKAGES_TOKEN" // the write credential (via secret ref)
)

// artifactTarget is one declared artifact, resolved for one run.
type artifactTarget struct {
	// Pattern is what the workflow wrote (a shell glob or a directory).
	Pattern string
	// Name is the artifact's name inside the run's path (a sanitised stem).
	Name string
}

// resolveArtifacts validates a job's artifact patterns.
//
// A pattern is rejected when it is empty or escapes the workspace: an artifact
// uploader must never be able to read `/etc` out of the container.
func resolveArtifacts(patterns []string) ([]artifactTarget, error) {
	out := make([]artifactTarget, 0, len(patterns))
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		// Absolute paths and `..` are refused: the uploader runs inside the
		// container with the job's own privileges, but an artifact must stay
		// inside /workspace so a workflow cannot exfiltrate the image's files.
		if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return nil, fmt.Errorf("ci: artifact %q must be a path inside /workspace", raw)
		}
		out = append(out, artifactTarget{Pattern: p, Name: artifactName(p)})
	}
	return out, nil
}

// artifactName turns a glob into a filesystem-safe artifact name.
//
// "bin/*" becomes "bin", "coverage/" becomes "coverage", "dist/app.tar.gz" stays.
func artifactName(pattern string) string {
	p := strings.TrimSuffix(strings.TrimSpace(pattern), "/")
	// Drop a trailing glob segment ("bin/*" -> "bin").
	if i := strings.LastIndex(p, "/"); i >= 0 {
		last := p[i+1:]
		if last == "*" || strings.ContainsAny(last, "*?[") {
			p = p[:i]
		}
	} else if p == "*" || strings.ContainsAny(p, "*?[") {
		p = "artifact"
	}
	p = strings.Trim(p, "/")
	if p == "" {
		p = "artifact"
	}
	// A name is used in a URL path. Keep dots (they are meaningful in a filename
	// like junit.xml) and replace only the separators that would change the URL
	// structure; everything else is sanitised per character.
	return urlSafe(strings.ReplaceAll(p, "/", "-"))
}

// artifactScript renders the upload block appended to the supervisor script.
//
// It runs only when the job SUCCEEDED (a failed build's half-written outputs are
// not worth keeping), and each pattern is uploaded independently so one missing
// glob does not lose the rest. A failed upload is reported but does not fail an
// otherwise green job: the artifacts are a convenience, and the build's verdict
// is what the merge gate acts on.
func artifactScript(job *WorkflowJob, run *Run, inst *Instance, targets []artifactTarget) string {
	if len(targets) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# ---- artifacts ----\n")
	b.WriteString("if [ \"${EASYDEV_FAILED:-0}\" = \"0\" ] && [ -n \"${EASYDEV_ARTIFACTS_URL:-}\" ]; then\n")
	b.WriteString("  easydev_artifact_upload() {\n")
	b.WriteString("    pattern=\"$1\"; name=\"$2\"\n")
	b.WriteString("    set -- $pattern\n")
	b.WriteString("    if [ ! -e \"$1\" ]; then\n")
	b.WriteString("      echo \"artifact $pattern: not found (skipped)\"\n")
	b.WriteString("      return 0\n")
	b.WriteString("    fi\n")
	b.WriteString("    url=\"$EASYDEV_ARTIFACTS_URL$EASYDEV_ARTIFACTS_PATH/$name.tgz\"\n")
	b.WriteString("    if tar czf - $pattern 2>/dev/null | \\\n")
	b.WriteString("         curl -fsS -X PUT --data-binary @- \\\n")
	b.WriteString("           -H \"Authorization: Bearer $EASYDEV_PACKAGES_TOKEN\" \"$url\" >/dev/null; then\n")
	b.WriteString("      echo \"artifact $name -> $url\"\n")
	b.WriteString("    else\n")
	b.WriteString("      echo \"artifact $pattern: upload failed\"\n")
	b.WriteString("    fi\n")
	b.WriteString("  }\n")
	for _, t := range targets {
		fmt.Fprintf(&b, "  easydev_artifact_upload %s %s\n", shellQuote(t.Pattern), shellQuote(t.Name))
	}
	b.WriteString("fi\n")
	return b.String()
}

// ArtifactBase resolves the registry base URL for a deployment.
//
// Empty means "artifacts are disabled": a job with `artifacts:` then logs that
// it skipped them rather than failing, because a deployment without a registry
// should still be able to run CI.
func (o *Orchestrator) artifactBase() string {
	return strings.TrimSuffix(o.artifactsBase, "/")
}

// WithArtifacts enables `artifacts:`.
func (o *Orchestrator) WithArtifacts(baseURL string) *Orchestrator {
	o.artifactsBase = baseURL
	return o
}

// artifactPath is the per-run directory an artifact is published under.
func artifactPath(run *Run, inst *Instance) string {
	return "/" + sanitize(run.Namespace) + "/" + sanitize(run.ID) + "/" + sanitize(inst.Key)
}

// ArtifactURL is the URL an uploaded artifact will live at (for the run record).
func ArtifactURL(base string, run *Run, inst *Instance, name string) string {
	return strings.TrimSuffix(base, "/") + ArtifactURLPrefix + artifactPath(run, inst) + "/" + name + ".tgz"
}

// withArtifactEnv adds the uploader's environment.
//
// It is only added when the job declares artifacts AND a registry is configured:
// an unrelated job should not carry a registry URL it will never use.
func withArtifactEnv(env map[string]string, base, path string, wanted bool) map[string]string {
	if !wanted || base == "" {
		if env == nil {
			return map[string]string{}
		}
		return env
	}
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		out[k] = v
	}
	out[EnvArtifactBase] = base
	out[EnvArtifactPath] = path
	return out
}

// urlSafe keeps [a-z0-9._-] and replaces anything else.
func urlSafe(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" || out == "." || out == ".." {
		return "artifact"
	}
	return out
}
