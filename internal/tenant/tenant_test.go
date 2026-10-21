package tenant

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNamespaceFromPath(t *testing.T) {
	cases := map[string]string{
		"/api/v1/team-a/tree":     "team-a",
		"/api/v1/team-a/ops/jobs": "team-a",
		"/api/v1/我的项目/tree":       "我的项目",
		"/api/v1/team-a":          "team-a",
		"/api/v1/":                "",
		"/other/team-a/tree":      "",
	}
	for path, want := range cases {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if got := Namespace(r); got != want {
			t.Fatalf("%s -> %q, want %q", path, got, want)
		}
	}
}

func TestUsernameHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/team-a/tree", nil)
	if got := Username(r); got != "" {
		t.Fatalf("no header -> %q, want empty", got)
	}
	r.Header.Set(UserHeader, " alice ")
	if got := Username(r); got != "alice" {
		t.Fatalf("header -> %q, want alice", got)
	}
}

// TestIdentityDeclinesWithoutUser pins the no-op guarantee: with no user header
// the resolver DECLINES, so easyvcs keeps its default (anonymous/token) path.
func TestIdentityDeclinesWithoutUser(t *testing.T) {
	rv := &Resolver{CS: nil} // no store needed when it declines early
	r := httptest.NewRequest(http.MethodGet, "/api/v1/team-a/tree", nil)
	if _, ok := rv.Identity(r); ok {
		t.Fatal("must decline when there is no user header")
	}
}
