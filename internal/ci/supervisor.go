package ci

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/easydev-org/easydev-platform/internal/ci/expr"
)

// Supervisor: ONE executor job per (job × matrix) instance, running all of its
// steps sequentially inside a single container.
//
// WHY: a step used to be its own executor job, which meant a fresh empty
// workspace per step — so "compile in step 1, test in step 2" could not work,
// every step re-downloaded the checkout, and (because executor names were
// truncated to 60 chars) two steps of the same job could even collide.
//
// The trade-off is that Go can no longer intervene between steps, so anything
// that depends on runtime state is decided IN the container:
//
//   - `if:` is compiled to a shell gate (see expr.ShellCompile);
//   - step outcomes are recorded in the script and reported at the END, in a
//     machine-readable block, so a truncated log still carries them.
//
// The script writes each step's body to its own file (base64-decoded) instead of
// interpolating it into a heredoc: a workflow may contain ANY text, so a literal
// heredoc delimiter would be an injection vector.

// resultMarker and outputMarker delimit the machine-readable blocks the
// orchestrator parses out of the supervisor's log.
const (
	resultMarker = "##[easydev:result]"
	outputMarker = "##[easydev:step-outputs]"
)

// stepPlan is one step compiled for the supervisor.
type stepPlan struct {
	index         int
	id            string
	name          string
	body          string // the script, already interpolated
	shell         string // bash | sh | pwsh
	workdir       string
	env           map[string]string
	gate          string // shell test; exits 0 when the step should run
	always        bool   // `if: always()` / a gate that ignores prior failure
	continueOnErr bool
	outputs       map[string]string // declared step outputs (reserved)
}

// supervisorScript renders the whole job as one shell script.
//
// The script is deterministic given the plan, so a re-run of the same snapshot
// produces the same bytes.
func supervisorScript(plan []stepPlan, checkout *CheckoutSpec, vcsUser, vcsToken, vcsBase, ns, artifacts string) string {
	var b strings.Builder
	b.WriteString("set -u\n")
	b.WriteString("mkdir -p /workspace/.easydev/out\n")
	b.WriteString("cd /workspace\n\n")

	// The checkout happens ONCE, before any step: every step sees the same tree,
	// and a step that modifies it affects the next one (that is the point).
	if checkout != nil {
		b.WriteString(checkoutBlock(checkout, vcsUser, vcsToken, vcsBase, ns))
	}

	// Helpers available to step gates.
	b.WriteString(`
easydev_out() {
  awk -F= -v k="$2" '$1==k { v=$0; sub("^[^=]*=","",v) } END { if (v!="") print v }' \
    "/workspace/.easydev/out/$1" 2>/dev/null
}
EASYDEV_FAILED=0
EASYDEV_CANCELLED=0
FIRST_FAIL_CODE=0

`)
	for _, sp := range plan {
		writeStep(&b, sp)
	}

	// Artifacts upload BEFORE the result block: the summary must be the last
	// thing in the log, or a truncated tail would lose it. A failed upload never
	// fails an otherwise green job (see artifactScript).
	if artifacts != "" {
		b.WriteString(artifacts)
	}

	// The result block is the LAST thing written, so a log truncated to its tail
	// still contains it.
	b.WriteString("echo '")
	b.WriteString(resultMarker)
	b.WriteString(" summary'\n")
	for _, sp := range plan {
		fmt.Fprintf(&b, "echo '%s %d '\"$EASYDEV_STEP_STATE_%d\"' '\"$EASYDEV_STEP_RC_%d\"''\n",
			resultMarker, sp.index, sp.index, sp.index)
	}
	// Per-step outputs, keyed by index.
	for _, sp := range plan {
		fmt.Fprintf(&b, "if [ -s /workspace/.easydev/out/%d ]; then\n", sp.index)
		fmt.Fprintf(&b, "  echo '%s %d'\n", outputMarker, sp.index)
		fmt.Fprintf(&b, "  cat /workspace/.easydev/out/%d\n", sp.index)
		b.WriteString("fi\n")
	}
	b.WriteString("\nexit \"$FIRST_FAIL_CODE\"\n")
	return b.String()
}

// writeStep renders one step: its gate, its body, and the bookkeeping that lets
// the orchestrator reconstruct the outcome.
func writeStep(b *strings.Builder, sp stepPlan) {
	idx := sp.index
	fmt.Fprintf(b, "# ---- step %d: %s ----\n", idx, oneLine(sp.name))

	// The gate decides whether the step runs. A step with no `if:` implicitly
	// requires every previous step to have SUCCEEDED (GitHub's documented
	// behaviour), which is what makes `if: always()` meaningful.
	fmt.Fprintf(b, "if { %s; }; then\n", sp.gate)

	// The body is written from base64 so no workflow text can terminate a heredoc.
	body := sp.body
	if sp.shell != "" && sp.shell != "bash" {
		body = scriptForShell(sp.shell, body)
	}
	enc := base64.StdEncoding.EncodeToString([]byte(body))
	fmt.Fprintf(b, "  printf '%%s' %s | base64 -d > /workspace/.easydev/step-%d.sh\n", shellQuote(enc), idx)

	fmt.Fprintf(b, "  (\n")
	for k, v := range sp.env {
		fmt.Fprintf(b, "    export %s=%s\n", k, shellQuote(v))
	}
	fmt.Fprintf(b, "    export EASYDEV_OUTPUT=/workspace/.easydev/out/%d\n", idx)
	fmt.Fprintf(b, "    : > \"$EASYDEV_OUTPUT\"\n")
	if sp.workdir != "" && sp.workdir != "." {
		fmt.Fprintf(b, "    cd %s\n", shellQuote(sp.workdir))
	}
	fmt.Fprintf(b, "    exec %s /workspace/.easydev/step-%d.sh\n", shellBinary(sp.shell), idx)
	fmt.Fprintf(b, "  )\n")
	fmt.Fprintf(b, "  EASYDEV_STEP_RC_%d=$?\n", idx)
	b.WriteString("else\n")
	fmt.Fprintf(b, "  EASYDEV_STEP_RC_%d=0\n", idx)
	fmt.Fprintf(b, "  export EASYDEV_STEP_STATE_%d=skipped\n", idx)
	b.WriteString("fi\n")

	// Bookkeeping: a failed step taints success() for everything after it, and
	// the FIRST failing exit code becomes the job's exit code.
	fmt.Fprintf(b, "if [ \"${EASYDEV_STEP_RC_%d}\" -ne 0 ]; then\n", idx)
	fmt.Fprintf(b, "  if [ \"${EASYDEV_STEP_STATE_%d:-}\" = \"skipped\" ]; then :\n", idx)
	fmt.Fprintf(b, "  else\n")
	fmt.Fprintf(b, "    export EASYDEV_STEP_STATE_%d=failed\n", idx)
	if sp.continueOnErr {
		// GitHub's semantics: a continue-on-error step reports failure but does
		// NOT fail the job, and success() stays true for later steps.
		b.WriteString("    EASYDEV_STEP_CONTINUED=1\n")
	} else {
		b.WriteString("    EASYDEV_FAILED=1\n")
		fmt.Fprintf(b, "    if [ \"$FIRST_FAIL_CODE\" = \"0\" ]; then FIRST_FAIL_CODE=$EASYDEV_STEP_RC_%d; fi\n", idx)
	}
	fmt.Fprintf(b, "  fi\n")
	fmt.Fprintf(b, "else\n")
	fmt.Fprintf(b, "  if [ \"${EASYDEV_STEP_STATE_%d:-}\" != \"skipped\" ]; then\n", idx)
	fmt.Fprintf(b, "    export EASYDEV_STEP_STATE_%d=success\n", idx)
	b.WriteString("  fi\n")
	b.WriteString("fi\n\n")
}

// checkoutBlock renders the one-time tree materialisation.
func checkoutBlock(c *CheckoutSpec, user, token, base, ns string) string {
	url := c.URL
	if url == "" {
		base = strings.TrimSuffix(base, "/")
		url = fmt.Sprintf("%s/api/v1/namespaces/%s/archive/%s.tar.gz", base, ns, c.Ref)
		if c.Path != "" {
			url += "?path=" + c.Path
		}
	}
	dest := c.Dest
	if dest == "" {
		dest = "."
	}
	var b strings.Builder
	b.WriteString("echo '::easydev:step checkout'\n")
	curl := "curl -fsSL"
	if user != "" || token != "" {
		// -u reads the credential from an ENV var, never from the script text, so
		// a token with shell metacharacters cannot break out.
		curl += ` -u "$EASYDEV_CHECKOUT_AUTH"`
	}
	b.WriteString("mkdir -p " + shellQuote(dest) + "\n")
	b.WriteString(curl + " " + shellQuote(url) + " | tar -xz -C " + shellQuote(dest) + "\n\n")
	return b.String()
}

// shellBinary is the interpreter for a step.
func shellBinary(shell string) string {
	switch strings.ToLower(shell) {
	case "pwsh", "powershell":
		return "pwsh"
	case "sh":
		return "/bin/sh"
	default:
		return "/bin/bash"
	}
}

// scriptForShell adds the matching strict-mode preamble to a non-POSIX shell.
func scriptForShell(shell, body string) string {
	switch strings.ToLower(shell) {
	case "pwsh", "powershell":
		return "$ErrorActionPreference = 'Stop'\n" + body
	}
	return body
}

// oneLine flattens a name for a comment.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "#", "")
}

// compileGate builds a step's shell gate from its `if:` expression.
func compileGate(ifExpr string, ctx *expr.Ctx, stepIndex map[string]int) (string, error) {
	if strings.TrimSpace(ifExpr) == "" {
		// No `if:` means "only if everything before succeeded".
		return `test "${EASYDEV_FAILED:-0}" = "0"`, nil
	}
	return expr.ShellCompile(ifExpr, ctx, stepIndex)
}

// DecodeScriptBodies extracts and decodes the base64 step bodies a supervisor
// script embeds. It exists so tests (and a future UI showing the effective
// script) can read a workflow's real content instead of its encoding.
func DecodeScriptBodies(script string) string {
	var out strings.Builder
	for _, line := range strings.Split(script, "\n") {
		const marker = "printf '%s' '"
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(marker):]
		j := strings.Index(rest, "'")
		if j < 0 {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(rest[:j]); err == nil {
			out.WriteString(string(b))
			out.WriteString("\n")
		}
	}
	return out.String()
}
