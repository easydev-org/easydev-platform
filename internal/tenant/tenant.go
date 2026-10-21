// Package tenant resolves the caller's TENANT (namespace) and ROLE for a
// request, and hands them to the embedded easyvcs server via its WithIdentity
// hook.
//
// For P2 the resolution is deliberately simple and explicit:
//   - the tenant is the FIRST path segment of /api/v1/{ns}/...;
//   - the user is taken from X-Easydev-User (a seam for P4's real auth);
//   - the role is derived from the easyvcs store ACL for that (ns,user) pair,
//     so a host identity never silently escalates.
//
// P4 replaces User-from-header with the unified token authority without
// changing this interface.
package tenant

import (
	"net/http"
	"strings"

	"github.com/easylab-platform/easyvcs/server"
	"github.com/easylab-platform/easyvcs/store"
)

// UserHeader carries the caller's identity in P2 (until the unified identity
// lands). It is a seam, not a trust boundary: the role is still checked against
// the store ACL.
const UserHeader = "X-Easydev-User"

// TenantHeader carries the resolved tenant, set by the gateway on the INTERNAL
// rewritten request. It matters because the gateway rewrites
// /api/v1/{ns}/... into easyvcs' own shape /api/v1/namespaces/{ns}/..., after
// which the tenant is no longer the first path segment. The gateway always
// OVERWRITES this header, so a client cannot forge it (and even if it could, the
// role is still resolved from the store ACL for that (tenant,user) pair).
const TenantHeader = "X-Easydev-Tenant"

// Resolver turns a request into (tenant, username).
type Resolver struct {
	CS *store.CentralStore
}

// Namespace extracts the tenant from the path: /api/v1/<ns>/...
func Namespace(r *http.Request) string {
	// The gateway sets TenantHeader on the internal (rewritten) request; prefer
	// it because the path is then easyvcs' own shape, not the platform's.
	if ns := strings.TrimSpace(r.Header.Get(TenantHeader)); ns != "" {
		return ns
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if rest == r.URL.Path || rest == "" {
		return ""
	}
	// easyvcs-native shape: /api/v1/namespaces/<ns>/...  -> the tenant is the
	// segment AFTER "namespaces".
	if after, ok := strings.CutPrefix(rest, "namespaces/"); ok {
		ns, _, _ := strings.Cut(after, "/")
		return ns
	}
	ns, _, _ := strings.Cut(rest, "/")
	return ns
}

// Username extracts the caller's name ("" = anonymous).
func Username(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(UserHeader))
}

// Identity implements easyvcs's server.IdentityResolver for the embedded VCS
// server: it resolves (user id, username, role) for the request's tenant.
//
// Declining (ok=false) leaves easyvcs on its default token path, so anonymous
// requests and the standalone behaviour are untouched.
func (rv *Resolver) Identity(r *http.Request) (server.Identity, bool) {
	name := Username(r)
	if name == "" {
		return server.Identity{}, false // anonymous: let easyvcs decide
	}
	ns := Namespace(r)
	if ns == "" {
		return server.Identity{}, false
	}
	u, err := rv.CS.GetUserByUsername(name)
	if err != nil || u == nil {
		// Unknown user: decline, so the request is treated as anonymous rather
		// than inventing a role.
		return server.Identity{}, false
	}
	role, err := rv.CS.RoleOfRef(store.NSRef{Name: ns}, u.ID)
	if err != nil {
		role = store.RoleNone
	}
	return server.Identity{UserID: u.ID, Username: u.Username, Role: role}, true
}
