package ci

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Permissions.
//
// A workflow declares what it needs:
//
//	permissions:
//	  contents: read        # the default: read the repo
//	  packages: write       # push to the package registry
//
// The declaration is ENFORCED, not decorative: it decides which credential the
// job receives. Without `contents: write` the job gets a READ-ONLY token (or no
// token at all), so a workflow cannot silently hold write access it never asked
// for — and a workflow that asks for a scope the platform cannot grant fails at
// plan time instead of running with less than it believes it has.
//
// This is a deliberately small model: the VCS exposes two token levels
// (read/write), so `contents` maps onto them and `packages` decides whether a
// registry credential is injected.

// Permission is the access level a scope grants.
type Permission string

const (
	PermNone  Permission = "none"
	PermRead  Permission = "read"
	PermWrite Permission = "write"
)

// knownScopes are the permission keys this platform understands.
//
// An UNKNOWN key is a hard error: a workflow that declares `security-events:
// write` must not silently run without it (a false sense of restriction) nor with
// undefined behaviour.
var knownScopes = map[string]bool{
	"contents": true, // the VCS: read the snapshot, push branches
	"packages": true, // the package registry (easyblobs)
	"checks":   true, // write back CI checks / statuses
}

// parsePermissions validates the declarations and applies the defaults.
//
// The default is NONE for every scope: a workflow that never mentions
// `permissions:` receives no credential at all, which is the least-privilege
// answer and keeps an undeclared workflow from silently holding repo access. A
// workflow that needs the repo declares `contents: read` (or `write`) and then
// must actually get it — see resolvePermissions.
func parsePermissions(decl map[string]string) (map[string]Permission, error) {
	out := map[string]Permission{
		"contents": PermNone,
		"packages": PermNone,
		"checks":   PermNone,
	}
	keys := make([]string, 0, len(decl))
	for k := range decl {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		key := strings.ToLower(strings.TrimSpace(k))
		if !knownScopes[key] {
			return nil, fmt.Errorf("ci: unknown permission %q (known: contents, packages, checks)", k)
		}
		switch strings.ToLower(strings.TrimSpace(decl[k])) {
		case "read":
			out[key] = PermRead
		case "write":
			out[key] = PermWrite
		case "none":
			out[key] = PermNone
		default:
			return nil, fmt.Errorf("ci: permission %q must be read, write or none (got %q)", k, decl[k])
		}
	}
	return out, nil
}

// EffectivePermissions returns a workflow's validated permissions.
func (w *Workflow) EffectivePermissions() (map[string]Permission, error) {
	return parsePermissions(w.Permissions)
}

// TokenMinter mints a scoped token for a run.
//
// It is an interface so the platform can back it with easyvcs and tests can use a
// stub. `write` distinguishes the two levels the VCS supports.
type TokenMinter interface {
	MintToken(ctx context.Context, tenant, label string, write bool) (string, error)
}

// permissionEnv names the env vars a job receives for each granted scope.
const (
	// EnvVCSReadToken is set when `contents: read` or `write` is granted.
	EnvVCSReadToken = "EASYDEV_VCS_TOKEN"
	// EnvVCSWriteToken is set ONLY when `contents: write` is granted.
	EnvVCSWriteToken = "EASYDEV_VCS_WRITE_TOKEN"
	// EnvPackagesToken is set ONLY when `packages: write` is granted.
	EnvPackagesToken = "EASYDEV_PACKAGES_TOKEN"
)

// resolvePermissions returns the env a job gets for its declared permissions.
//
// A minted token is returned as a REFERENCE (the caller stores it in the run's
// secret), never as a literal, so the same rule as `secrets:` applies: a
// credential never enters the job spec.
func (o *Orchestrator) resolvePermissions(ctx context.Context, run *Run, wf *Workflow) (map[string]string, error) {
	perms, err := wf.EffectivePermissions()
	if err != nil {
		return nil, err
	}
	wantWrite := perms["contents"] == PermWrite
	wantRead := perms["contents"] != PermNone
	wantPackages := perms["packages"] != PermNone

	if !wantRead && !wantPackages {
		return nil, nil // nothing declared: no credential, nothing to leak
	}
	if o.tokens == nil {
		// The workflow asked for access the platform cannot provide: fail rather
		// than run without the credential it declared.
		return nil, fmt.Errorf("ci: the workflow requests %s but no token minter is configured", describePerms(perms))
	}
	out := map[string]string{}
	if wantRead {
		tok, terr := o.tokens.MintToken(ctx, run.Namespace, "ci-"+run.ID, wantWrite)
		if terr != nil {
			return nil, fmt.Errorf("ci: mint %s token: %w", permWord(wantWrite), terr)
		}
		// The read-only token is always available as the "lower" credential.
		out[EnvVCSReadToken] = tok
		if wantWrite {
			out[EnvVCSWriteToken] = tok
		}
	}
	if wantPackages {
		// The package registry shares the VCS identity (one token authority), so
		// the same credential is reused rather than minting a second one.
		if t, ok := out[EnvVCSWriteToken]; ok {
			out[EnvPackagesToken] = t
		} else if o.tokens != nil {
			tok, terr := o.tokens.MintToken(ctx, run.Namespace, "ci-"+run.ID+"-packages", true)
			if terr != nil {
				return nil, fmt.Errorf("ci: mint packages token: %w", terr)
			}
			out[EnvPackagesToken] = tok
		}
	}
	return out, nil
}

// describePerms renders the effective permissions for an error message.
func describePerms(p map[string]Permission) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if p[k] != PermNone {
			parts = append(parts, k+" "+string(p[k]))
		}
	}
	return strings.Join(parts, ", ")
}

func permWord(write bool) string {
	if write {
		return "write"
	}
	return "read"
}
