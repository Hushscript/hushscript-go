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

	missingTokenMessage = "Set HUSHSCRIPT_TOKEN or pass a token. Create one in the Hushscript app: " +
		"Account → Connected apps (app.hushscript.com/account/mcp)."
)

// config is the resolved configuration of a Client.
type config struct {
	token      secret
	baseURL    string
	baseHost   string
	timeout    time.Duration
	maxRetries int
	// maxRetriesSet tells an explicit 0 (retries off) from an option that was never given.
	maxRetriesSet bool
	logger        *slog.Logger
	clock         Clock
	httpClient    *http.Client
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
		validated, host, err := validateBaseURL(baseURL)
		if err != nil {
			return err
		}
		c.baseURL, c.baseHost = validated, host
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
// contexts), and does not stop it following redirects.
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
	return newSDKError(KindConfiguration, "config_invalid", message, nil, redactor{})
}

func validateBaseURL(raw string) (validated, host string, err error) {
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "", "", newConfigError("The base URL is not a valid URL.")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		switch parsed.Hostname() {
		case "localhost", "127.0.0.1", "::1":
		default:
			return "", "", newConfigError("The base URL must be https, or http to localhost, 127.0.0.1 or [::1].")
		}
	default:
		return "", "", newConfigError("The base URL must be https, or http to localhost, 127.0.0.1 or [::1].")
	}
	if parsed.User != nil {
		return "", "", newConfigError("The base URL cannot carry credentials.")
	}
	return strings.TrimRight(parsed.String(), "/"), parsed.Host, nil
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
		return config{}, newSDKError(KindConfiguration, "config_invalid", missingTokenMessage, nil, redactor{})
	}
	if !strings.HasPrefix(c.token.reveal(), tokenPrefix) {
		return config{}, newConfigError("This doesn't look like a Hushscript token.")
	}
	if c.baseURL == "" {
		base := os.Getenv("HUSHSCRIPT_BASE_URL")
		if base == "" {
			base = defaultBaseURL
		}
		validated, host, err := validateBaseURL(base)
		if err != nil {
			return config{}, err
		}
		c.baseURL, c.baseHost = validated, host
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
	if c.httpClient == nil {
		c.httpClient = &http.Client{
			// The token must not follow a redirect anywhere, and the API never redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if err := resolveLogger(&c); err != nil {
		return config{}, err
	}
	return c, nil
}
