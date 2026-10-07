package hushscript

import (
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

// urlQuery finds a URL with a query string. The query of a presigned storage URL is a signature, so
// no text the SDK shows keeps one (spec section 10).
var urlQuery = regexp.MustCompile(`(https?://[^\s"'<>?#]+)\?[^\s"'<>#]*`)

// redactedQuery replaces a URL's query in text.
const redactedQuery = "…[redacted]"

// redactor removes the client's token, and anything shaped like a token, from text.
type redactor struct {
	token secret
	// past holds the tokens a rotation replaced.
	past []secret
}

func newRedactor(token string) redactor { return redactor{token: newSecret(token)} }

func (r redactor) apply(text string) string {
	for _, held := range append([]secret{r.token}, r.past...) {
		token := held.reveal()
		if token == "" {
			continue
		}
		text = strings.ReplaceAll(text, token, redactedToken)
		// An API that echoes only what follows "pat." (the id and the secret), or only the secret
		// segment after the last dot, is just as much a leak.
		if _, tail, found := strings.Cut(token, "pat."); found {
			for _, part := range []string{tail, tail[strings.LastIndex(tail, ".")+1:]} {
				if len(part) >= minSecretTail {
					text = strings.ReplaceAll(text, part, redactedToken)
				}
			}
		}
	}
	text = tokenPattern.ReplaceAllString(text, redactedToken)
	return urlQuery.ReplaceAllString(text, "${1}?"+redactedQuery)
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
