package expr

import (
	"strings"
	"testing"
)

func TestInterpolateVariables(t *testing.T) {
	ctx := &Ctx{Vars: map[string]any{
		"github": map[string]any{"ref": "main", "rr": float64(7)},
		"matrix": map[string]any{"go": "1.27"},
		"env":    map[string]any{"MSG": "hello"},
	}}
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"ref=${{ github.ref }}", "ref=main"},
		{"go${{ matrix.go }}", "go1.27"},
		{"${{ env.MSG }} world", "hello world"},
		{"rr#${{ github.rr }}", "rr#7"},
		{"${{ 'a}}b' }}", "a}}b"},
		{"${{github.ref}}", "main"},
		{"lit=${{ 'x y' }}", "lit=x y"},
		{"missing=${{ nope.nope }}", "missing="},
	}
	for _, c := range cases {
		got, err := Interpolate(c.in, ctx)
		if err != nil {
			t.Fatalf("Interpolate(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Interpolate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInterpolateErrors(t *testing.T) {
	ctx := &Ctx{}
	for _, in := range []string{"${{ 1 +", "${{ github.ref }}"[:4], "${{ unknownFunc(1) }}"} {
		if _, err := Interpolate(in, ctx); err == nil {
			t.Errorf("Interpolate(%q) should fail", in)
		}
	}
}

func TestEvalBool(t *testing.T) {
	ctx := &Ctx{Vars: map[string]any{
		"easyvcs": map[string]any{"trigger": "tag", "rr": float64(3)},
	}, Status: StatusSuccess}

	yes := []string{
		"", "true", "1", "!false",
		"easyvcs.trigger == 'tag'",
		"easyvcs.trigger != 'manual'",
		"easyvcs.rr > 2 && easyvcs.rr < 4",
		"success()", "always()",
		"contains(fromJSON('[\"a\",\"b\"]'), 'a')",
		"startsWith(easyvcs.trigger, 'ta')",
		"endsWith(easyvcs.trigger, 'ag')",
		"format('{0}-{1}', 'a', 2) == 'a-2'",
	}
	for _, e := range yes {
		got, err := EvalBool(e, ctx)
		if err != nil {
			t.Fatalf("EvalBool(%q): %v", e, err)
		}
		if !got {
			t.Errorf("EvalBool(%q) = false, want true", e)
		}
	}

	no := []string{
		"false", "0", "!true",
		"easyvcs.trigger == 'manual'",
		"failure()", "cancelled()",
		"contains('abc', 'z')",
		"easyvcs.rr >= 4",
		"null", "''",
	}
	for _, e := range no {
		got, err := EvalBool(e, ctx)
		if err != nil {
			t.Fatalf("EvalBool(%q): %v", e, err)
		}
		if got {
			t.Errorf("EvalBool(%q) = true, want false", e)
		}
	}
}

func TestStatusFunctions(t *testing.T) {
	for st, want := range map[Status]string{
		StatusSuccess:   "success",
		StatusFailure:   "failure",
		StatusCancelled: "cancelled",
	} {
		ctx := &Ctx{Status: st}
		for fn, expect := range map[string]bool{
			"success()": want == "success", "failure()": want == "failure",
			"cancelled()": want == "cancelled", "always()": true,
		} {
			got, err := EvalBool(fn, ctx)
			if err != nil {
				t.Fatalf("%s: %v", fn, err)
			}
			if got != expect {
				t.Errorf("status=%s %s = %v, want %v", st, fn, got, expect)
			}
		}
	}
}

func TestComparisonIsCaseInsensitiveForStrings(t *testing.T) {
	ctx := &Ctx{Vars: map[string]any{"x": "Main"}}
	got, err := EvalBool("x == 'main'", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("string comparison should be case-insensitive")
	}
}

func TestToStringAndJSON(t *testing.T) {
	cases := []struct {
		v    any
		want string
	}{
		{nil, ""},
		{true, "true"},
		{float64(3), "3"},
		{float64(3.5), "3.5"},
		{[]any{"a", "b"}, "a,b"},
		{"s", "s"},
	}
	for _, c := range cases {
		if got := ToString(c.v); got != c.want {
			t.Errorf("ToString(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

func TestFromJSONRoundTrip(t *testing.T) {
	v, err := fromJSON(`{"a":[1,2],"b":"x","c":true,"d":null}`)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("got %T", v)
	}
	if list, ok := m["a"].([]any); !ok || len(list) != 2 {
		t.Fatalf("a = %#v", m["a"])
	}
	if got := toJSON(m); !strings.Contains(got, `"a":[1,2]`) {
		t.Errorf("toJSON = %s", got)
	}
}

func TestFormatEscapes(t *testing.T) {
	got, err := format("{{0}} {0} {1}", []any{"a", 2})
	if err != nil {
		t.Fatal(err)
	}
	if got != "{0} a 2" {
		t.Errorf("format = %q", got)
	}
	if _, err := format("{2}", []any{"a"}); err == nil {
		t.Error("a missing placeholder index must error")
	}
}
