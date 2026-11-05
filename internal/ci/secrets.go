package ci

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Secrets.
//
// A workflow refers to a secret as `${{ secrets.NAME }}`. The value is NEVER
// interpolated into the script (that would put it in the Job spec, the log, and
// the expression machinery). Instead:
//
//   - every `secrets.NAME` reference in a step's `env:`/body becomes a
//     SecretEnvVar pointing at a Secret key, which the kubelet injects;
//   - the script reads it from the environment, so the value exists only inside
//     the container.
//
// The Secret itself is created per run from the tenant's stored secrets, named
// after the run, and removed with the job. That keeps one tenant's secrets out of
// another's pods and out of a long-lived object.
//
// A reference to a secret the tenant does not have FAILS THE RUN at plan time:
// silently substituting "" would make a deploy run with an empty token.

// secretPrefix is the env-var prefix for injected secrets, so a workflow reads
// `$EASYDEV_SECRET_DEPLOY_TOKEN`.
const secretPrefix = "EASYDEV_SECRET_"

// SecretsProvider resolves a tenant's stored secrets by name.
//
// It is an interface so the platform can back it with easyops (PutSecret/
// GetSecret) and tests can use a map. `nil` disables secrets: any workflow that
// references one then fails with a clear error instead of running without it.
type SecretsProvider interface {
	// SecretValues returns the key/value pairs of a tenant secret.
	SecretValues(ctx context.Context, tenant, name string) (map[string]string, error)
}

// secretRefs collects the `secrets.X` names a workflow references.
func secretRefs(w *Workflow) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		for _, name := range scanSecrets(s) {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	for _, j := range w.Jobs {
		for _, st := range j.Steps {
			add(st.Run)
			add(st.If)
			add(st.Uses)
			for _, v := range st.Env {
				add(v)
			}
			for _, v := range j.Env {
				add(v)
			}
			for _, v := range w.Env {
				add(v)
			}
		}
	}
	sort.Strings(out)
	return out
}

// scanSecrets finds `secrets.NAME` occurrences in a template string.
func scanSecrets(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, "secrets.")
		if i < 0 {
			return out
		}
		rest = rest[i+len("secrets."):]
		j := 0
		for j < len(rest) && (rest[j] == '_' || rest[j] == '-' ||
			(rest[j] >= 'a' && rest[j] <= 'z') || (rest[j] >= 'A' && rest[j] <= 'Z') ||
			(rest[j] >= '0' && rest[j] <= '9')) {
			j++
		}
		if j > 0 {
			out = append(out, rest[:j])
		}
	}
}

// resolveSecrets fetches every secret a workflow needs and returns the flat
// env-var map plus the references the executor must resolve.
//
// A missing provider and a missing secret are both hard errors: a run must not
// proceed with an empty credential.
func (o *Orchestrator) resolveSecrets(ctx context.Context, run *Run, wf *Workflow) (map[string]string, []SecretEnvVar, map[string]any, error) {
	names := secretRefs(wf)
	if len(names) == 0 {
		return nil, nil, map[string]any{}, nil
	}
	if o.secrets == nil {
		return nil, nil, nil, fmt.Errorf("ci: the workflow uses %s but no secrets provider is configured",
			strings.Join(names, ", "))
	}
	env := map[string]string{}
	var refs []SecretEnvVar
	// The `secrets` context the expression language sees: the NAME of each
	// secret's env var, so `${{ secrets.X }}` interpolates to a shell variable
	// reference instead of the value.
	ctxVars := map[string]any{}

	// One Secret per run holds every value the workflow needs, so the job spec
	// carries references into a single object.
	secretName := runSecretName(run)
	for _, name := range names {
		values, err := o.secrets.SecretValues(ctx, run.Namespace, name)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("ci: secret %q: %w", name, err)
		}
		if len(values) == 0 {
			return nil, nil, nil, fmt.Errorf("ci: secret %q is empty or does not exist", name)
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			envKey := secretPrefix + strings.ToUpper(strings.ReplaceAll(name+"_"+k, "-", "_"))
			refs = append(refs, SecretEnvVar{
				Env: envKey, Secret: secretName,
				// The key encodes the secret name so two secrets can both have a
				// `token` key.
				Key: secretKey(name, k),
			})
			env[envKey] = values[k]
			// `${{ secrets.X }}` becomes a shell EXPANSION, so the value is read
			// from the environment at run time and never interpolated.
			ctxVars[name] = "${" + envKey + "}"
		}
	}
	return env, refs, ctxVars, nil
}

// runSecretName is the per-run Secret object name.
func runSecretName(run *Run) string {
	return sanitize("ci-" + run.ID)
}

// secretKey is the key inside the run Secret.
func secretKey(secret, key string) string {
	return sanitize(secret + "-" + key)
}

// secretsContext makes `secrets.X` resolve to a shell expansion.
//
// The expression engine's `secrets` context is normally empty; populating it with
// "${EASYDEV_SECRET_...}" strings means an interpolated `${{ secrets.NPM_TOKEN }}`
// becomes a shell variable reference, evaluated inside the container — the value
// is never in the script.
func secretsContext(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	return values
}
