package hushscript

import (
	"strings"
	"testing"
)

func TestRedactsTheTokenAndAnythingShapedLikeOne(t *testing.T) {
	r := redactor{token: testToken}
	other := "hsr2:us:pat.deadbeef.cafe0123"
	in := "token " + testToken + " and " + other + " and the bare secret " + testSecret
	out := r.apply(in)
	for _, leaked := range []string{testToken, other, testSecret, "pat."} {
		if strings.Contains(out, leaked) {
			t.Errorf("%q survived in %q", leaked, out)
		}
	}
	if strings.Count(out, "hsr…[redacted]") < 2 {
		t.Errorf("expected the marker at least twice: %q", out)
	}
}

func TestRedactsATokenThatDoesNotFollowTheCurrentFormat(t *testing.T) {
	r := redactor{token: "hsr-future-format-token"} //nolint:gosec // a made-up token for the test
	if out := r.apply("echo hsr-future-format-token end"); strings.Contains(out, "future-format") {
		t.Errorf("exact token survived: %q", out)
	}
}

func TestRedactionLeavesOrdinaryTextAlone(t *testing.T) {
	r := redactor{token: testToken}
	for _, text := range []string{"", "plain text", "hsr is a prefix, nothing more", "pat.abc.def"} {
		if got := r.apply(text); got != text {
			t.Errorf("%q changed to %q", text, got)
		}
	}
	if got := (redactor{}).apply("hsr1:eu:pat.00.ff"); got != "hsr…[redacted]" {
		t.Errorf("a redactor without a token still strips the pattern: %q", got)
	}
}

func TestRedactsInsideNestedValues(t *testing.T) {
	r := redactor{token: testToken}
	in := map[string]any{
		"a": testToken,
		"b": []any{testToken, 7, map[string]any{"c": "x " + testToken}},
		"d": 1.5,
	}
	out := r.value(in)
	text := strings.Join([]string{asText(out)}, "")
	if strings.Contains(text, testSecret) {
		t.Errorf("nested token survived: %s", text)
	}
	if in["a"] != testToken {
		t.Error("the input was modified")
	}
}
