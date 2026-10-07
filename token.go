package hushscript

import "sync"

// tokenBox holds the token a Client sends. It is shared by pointer between the config copies the
// doer and the core keep, so a rotation reaches every later attempt, queued retries included
// (spec section 7.4). It also remembers the tokens it replaced, so redaction still covers them.
type tokenBox struct {
	mu      sync.RWMutex
	current string
	past    []string
}

func newTokenBox(token string) *tokenBox { return &tokenBox{current: token} }

func (b *tokenBox) get() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.current
}

// swap makes next the token every later attempt sends.
func (b *tokenBox) swap(next string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current != "" {
		b.past = append(b.past, b.current)
	}
	b.current = next
}

func (b *tokenBox) redactor() redactor {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return redactor{token: b.current, past: append([]string(nil), b.past...)}
}

// acceptableToken is the check every token passes, at construction and after a rotation: printable
// ASCII only, and the hsr prefix.
func acceptableToken(token string) bool {
	return validToken(token) && len(token) >= len(tokenPrefix) && token[:len(tokenPrefix)] == tokenPrefix
}
