package hushscript

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL    = "https://api.hushscript.com"
	defaultTimeout    = 60 * time.Second
	defaultMaxRetries = 3
	tokenPrefix       = "hsr"

	missingTokenMessage = "No Hushscript token: set HUSHSCRIPT_TOKEN or pass a token."
	// missingTokenHint is the one configuration hint (spec section 3); other configuration errors have none.
	missingTokenHint = "Set HUSHSCRIPT_TOKEN or pass a token. Create one in the Hushscript app: " +
		"Account → Connected apps (app.hushscript.com/account/mcp)."
)

// config is the resolved configuration of a Client.
type config struct {
	token      secret
	baseURL    string
	baseHost   string
	baseScheme string
	timeout    time.Duration
	maxRetries int
	// maxRetriesSet tells an explicit 0 (retries off) from an option that was never given.
	maxRetriesSet bool
	logger        *slog.Logger
	// logLevel is "off" or "debug" when WithLogLevel set it; "" leaves the choice to the environment.
	logLevel   string
	clock      Clock
	httpClient *http.Client
	// jitter picks the actual backoff below its ceiling. Tests replace it.
	jitter func(time.Duration) time.Duration
}

func (c config) String() string {
	return fmt.Sprintf("hushscript.config{baseURL: %s, timeout: %s, maxRetries: %d, token: %s}",
		c.baseURL, c.timeout, c.maxRetries, redactedToken)
}

func (c config) GoString() string { return c.String() }

// Format covers every fmt verb, including %d and %x, which would otherwise print the struct fields.
func (c config) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

func (c config) redactor() redactor { return redactor{token: c.token.reveal()} }

// Option configures New.
type Option func(*config) error

// WithToken sets the API token. Without it, New reads HUSHSCRIPT_TOKEN.
func WithToken(token string) Option {
	return func(c *config) error {
		c.token = secret(token)
		return nil
	}
}

// WithBaseURL sets the API host. It must be https, or http to localhost, 127.0.0.1 or [::1] (the
// conformance mock). Without it, New reads HUSHSCRIPT_BASE_URL and then uses https://api.hushscript.com.
func WithBaseURL(baseURL string) Option {
	return func(c *config) error {
		validated, err := validateBaseURL(baseURL)
		if err != nil {
			return err
		}
		c.setBase(validated)
		return nil
	}
}

// WithTimeout sets the per-request timeout (default 60 s). Each attempt of a retried request gets its own.
func WithTimeout(timeout time.Duration) Option {
	return func(c *config) error {
		if timeout <= 0 {
			return newConfigError("The timeout must be positive.")
		}
		c.timeout = timeout
		return nil
	}
}

// WithMaxRetries sets how many times a request is retried after the first attempt (default 3). It
// caps every retry rule; 0 turns retries off.
func WithMaxRetries(retries int) Option {
	return func(c *config) error {
		if retries < 0 {
			return newConfigError("maxRetries cannot be negative.")
		}
		c.maxRetries, c.maxRetriesSet = retries, true
		return nil
	}
}

// WithLogger turns on one debug line per request attempt: method, path, status, attempt, duration
// and retry delay. Never headers, queries, bodies or titles. Without it, HUSHSCRIPT_LOG=debug logs
// to standard error.
func WithLogger(logger *slog.Logger) Option {
	return func(c *config) error {
		c.logger = logger
		return nil
	}
}

// WithLogLevel sets the log level to "off" or "debug". It beats HUSHSCRIPT_LOG, so WithLogLevel("off")
// silences HUSHSCRIPT_LOG=debug. "debug" without WithLogger logs to standard error.
func WithLogLevel(level string) Option {
	return func(c *config) error {
		if level != logOff && level != logDebug {
			return newConfigError("The log level must be off or debug.")
		}
		c.logLevel = level
		return nil
	}
}

// WithClock replaces the clock the SDK sleeps on, for tests and the conformance runner.
func WithClock(clock Clock) Option {
	return func(c *config) error {
		if clock == nil {
			return newConfigError("The clock cannot be nil.")
		}
		c.clock = clock
		return nil
	}
}

// WithHTTPClient replaces the http.Client the SDK sends requests with, for a proxy, custom TLS roots
// or a counting transport. The SDK sets no Client.Timeout of its own on it (timeouts go through
// contexts). It sends through a shallow copy that never follows redirects, so the caller's client is
// left as it was and a 3xx answer is an error, whatever CheckRedirect the caller set.
func WithHTTPClient(client *http.Client) Option {
	return func(c *config) error {
		if client == nil {
			return newConfigError("The HTTP client cannot be nil.")
		}
		c.httpClient = client
		return nil
	}
}

func newConfigError(message string) *Error {
	return newSDKError(KindConfiguration, "config_invalid", message, nil, redactor{}, hintInputs{})
}

// setBase stores a validated base URL and the parts of it that decide where the token may go.
func (c *config) setBase(validated *url.URL) {
	c.baseURL = validated.String()
	c.baseHost = validated.Host
	c.baseScheme = validated.Scheme
}

// validateBaseURL accepts an https host, or an http one on loopback, with no path, query or fragment.
func validateBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, newConfigError("The base URL is not a valid URL.")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		switch parsed.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return nil, newConfigError("The base URL must be https, or http to localhost, 127.0.0.1 or [::1].")
		}
	default:
		return nil, newConfigError("The base URL must be https, or http to localhost, 127.0.0.1 or [::1].")
	}
	if parsed.User != nil {
		return nil, newConfigError("The base URL cannot carry credentials.")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, newConfigError("The base URL must be a bare host: no path, query or fragment.")
	}
	return &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}, nil
}

// validToken reports whether every byte is printable ASCII (0x21 to 0x7E), so the token can sit in
// a header and never carries a space, control character or non-ASCII byte.
func validToken(token string) bool {
	for i := range len(token) {
		if token[i] < 0x21 || token[i] > 0x7e {
			return false
		}
	}
	return true
}

// resolve applies the options, then the environment, then the defaults, and validates the result.
func resolve(opts []Option) (config, error) {
	var c config
	for _, opt := range opts {
		if err := opt(&c); err != nil {
			return config{}, err
		}
	}
	if c.token == "" {
		c.token = secret(os.Getenv("HUSHSCRIPT_TOKEN"))
	}
	if c.token == "" {
		failure := newConfigError(missingTokenMessage)
		failure.Hint = missingTokenHint
		return config{}, failure
	}
	if !validToken(c.token.reveal()) {
		return config{}, newConfigError("The token contains characters a Hushscript token never has (printable ASCII only).")
	}
	if !strings.HasPrefix(c.token.reveal(), tokenPrefix) {
		return config{}, newConfigError("This doesn't look like a Hushscript token.")
	}
	if c.baseURL == "" {
		base := os.Getenv("HUSHSCRIPT_BASE_URL")
		if base == "" {
			base = defaultBaseURL
		}
		validated, err := validateBaseURL(base)
		if err != nil {
			return config{}, err
		}
		c.setBase(validated)
	}
	if c.timeout == 0 {
		c.timeout = defaultTimeout
	}
	if !c.maxRetriesSet {
		c.maxRetries = defaultMaxRetries
	}
	if c.clock == nil {
		c.clock = systemClock{}
	}
	if c.jitter == nil {
		c.jitter = fullJitter
	}
	// The token must not follow a redirect anywhere, and the API never redirects: a 3xx comes back
	// as the answer, and the doer turns it into an error.
	noRedirects := &http.Client{}
	if c.httpClient != nil {
		copied := *c.httpClient
		noRedirects = &copied
	}
	noRedirects.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.httpClient = noRedirects
	if err := resolveLogger(&c); err != nil {
		return config{}, err
	}
	return c, nil
}
