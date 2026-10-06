package hushscript

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

const (
	redactedToken = "hsr…[redacted]" //nolint:gosec // the replacement marker, not a credential
	// minSecretTail keeps a short, ordinary substring from being redacted everywhere.
	minSecretTail = 16
)

// tokenPattern is spec section 10: the shape of a Hushscript token, wherever it turns up.
var tokenPattern = regexp.MustCompile(`hsr\d+:[a-z]+:pat\.[0-9a-f]+\.[0-9a-f]+`)

// secret holds the token. Every way of printing it shows the redaction marker instead.
type secret string

func (s secret) reveal() string { return string(s) }

func (secret) String() string { return redactedToken }

func (secret) GoString() string { return redactedToken }

func (secret) LogValue() slog.Value { return slog.StringValue(redactedToken) }

// Format covers every fmt verb, including the ones that bypass String (%x, %d).
func (secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redactedToken))
}

// redactor removes the client's token, and anything shaped like a token, from text.
type redactor struct {
	token string
}

func (r redactor) apply(text string) string {
	if r.token != "" {
		text = strings.ReplaceAll(text, r.token, redactedToken)
		// An API that echoes only the secret half is just as much a leak.
		if _, tail, found := strings.Cut(r.token, "pat."); found && len(tail) >= minSecretTail {
			text = strings.ReplaceAll(text, tail, redactedToken)
		}
	}
	return tokenPattern.ReplaceAllString(text, redactedToken)
}

// value redacts every string inside a decoded JSON value and returns a copy.
func (r redactor) value(v any) any {
	switch typed := v.(type) {
	case string:
		return r.apply(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = r.value(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[r.apply(key)] = r.value(item)
		}
		return out
	default:
		return v
	}
}
