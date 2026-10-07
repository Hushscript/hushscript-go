package hushscript

import (
	"fmt"
	"log/slog"
)

// secret is how the SDK holds a token, or a signed URL, at rest. The text lives in a closure, so no
// field of any struct that holds a secret is a string, a byte slice or a pointer to either: fmt,
// spew, testify's diff and the encoders all see a function value and print an address, whatever
// verb or configuration they use. Methods alone could not do this, because fmt skips the methods of
// an unexported field and spew with DisableMethods (testify's setting) skips them everywhere.
// Only unsafe reflection or a memory dump reaches the text; reveal is the one way in.
//
// The zero secret is empty.
type secret struct {
	text func() string
}

func newSecret(text string) secret {
	if text == "" {
		return secret{}
	}
	return secret{text: func() string { return text }}
}

func (s secret) reveal() string {
	if s.text == nil {
		return ""
	}
	return s.text()
}

func (s secret) isEmpty() bool { return s.text == nil }

func (secret) String() string { return redactedToken }

func (secret) GoString() string { return redactedToken }

func (secret) LogValue() slog.Value { return slog.StringValue(redactedToken) }

// Format covers every fmt verb, including the ones that bypass String (%x, %d).
func (secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redactedToken))
}

func (secret) MarshalText() ([]byte, error) { return []byte(redactedToken), nil }

func (secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedToken + `"`), nil }

func (secret) GobEncode() ([]byte, error) { return []byte(redactedToken), nil }
