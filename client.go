package hushscript

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/Hushscript/hushscript-sdk-go/generated"
)

const modulePath = "github.com/Hushscript/hushscript-sdk-go"

// Client is the entry point of the SDK. It is safe for concurrent use.
type Client struct {
	// Jobs lists, reads and waits for processing jobs.
	Jobs JobsService
	// Transcripts lists and reads finished transcripts.
	Transcripts TranscriptsService
	// Billing lists packs and cards, buys minutes and reads the purchase history.
	Billing BillingService
	// Agent rotates the token of an agent account. Signup and claim need no token: see AgentSignup
	// and AgentClaim.
	Agent AgentService

	core *core
}

// core holds everything a Client needs. Client points at it, so printing a Client value shows an
// address and never the configuration.
type core struct {
	cfg  config
	doer *doer
	api  *generated.ClientWithResponses

	// uploadTrace is a test seam: the uploader reports its fallback decisions through it.
	uploadTrace func(event string, part int)
}

// New builds a Client from the options, the environment and the defaults (see the With options).
// It returns a *Error of kind KindConfiguration when the configuration is unusable.
func New(opts ...Option) (*Client, error) {
	cfg, err := resolve(opts)
	if err != nil {
		return nil, err
	}
	c, err := newCore(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		Jobs: JobsService{core: c}, Transcripts: TranscriptsService{core: c},
		Billing: BillingService{core: c}, Agent: AgentService{core: c}, core: c,
	}, nil
}

func newCore(cfg config) (*core, error) {
	d := newDoer(cfg)
	api, err := generated.NewClientWithResponses(cfg.baseURL, generated.WithHTTPClient(d))
	if err != nil {
		return nil, newSDKError(KindConfiguration, "config_invalid", "The generated client could not be built.", err, cfg.redactor(), hintInputs{})
	}
	return &core{cfg: cfg, doer: d, api: api}, nil
}

// API returns the generated client for every /v1 operation, sharing the SDK's retries, timeouts and
// error handling. On a zero Client (one not built by New) it returns a client whose every call fails
// with a KindConfiguration error, so a forgotten New shows up as an error and not a nil dereference.
func (c Client) API() *generated.ClientWithResponses {
	if c.core == nil {
		return unconfiguredAPI()
	}
	return c.core.api
}

// unconfiguredDoer fails every request: it stands behind the API of a zero Client.
type unconfiguredDoer struct{}

func (unconfiguredDoer) Do(*http.Request) (*http.Response, error) {
	return nil, newConfigError("This Client was not built by hushscript.New.")
}

func unconfiguredAPI() *generated.ClientWithResponses {
	api, err := generated.NewClientWithResponses(defaultBaseURL, generated.WithHTTPClient(unconfiguredDoer{}))
	if err != nil { // the base URL is a constant, so this cannot happen
		panic(err)
	}
	return api
}

// String describes the client without its token.
func (c Client) String() string {
	if c.core == nil {
		return "hushscript.Client{unconfigured}"
	}
	cfg := c.core.cfg
	return fmt.Sprintf("hushscript.Client{baseURL: %s, timeout: %s, maxRetries: %d, token: %s}",
		cfg.baseURL, cfg.timeout, cfg.maxRetries, redactedToken)
}

// GoString is String, so %#v never shows the token either.
func (c Client) GoString() string { return c.String() }

// Format covers every fmt verb, including %d and %x, which would otherwise print the struct fields.
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// LogValue is the client as slog logs it: the same text as String, never the struct. It is on the
// pointer, so a nil *Client logs as "<nil>" and does not panic.
func (c *Client) LogValue() slog.Value {
	if c == nil {
		return slog.StringValue("<nil>")
	}
	return slog.StringValue(c.String())
}

var goVersionPattern = regexp.MustCompile(`go\d+\.\d+(?:\.\d+)?`)

// userAgent is "hushscript-go/<version> go/<go version>" (spec section 3).
func userAgent() string { return userAgentForVersion(runtime.Version()) }

// userAgentForVersion takes the runtime version as an argument so a test can pass any toolchain's. A
// toolchain without a numeric version (a development build) reports "devel".
func userAgentForVersion(runtimeVersion string) string {
	version := strings.TrimPrefix(goVersionPattern.FindString(runtimeVersion), "go")
	if version == "" {
		version = "devel"
	}
	return fmt.Sprintf("hushscript-go/%s go/%s", sdkVersion(), version)
}

// sdkVersion is the module version the consumer's build resolved, which release-please tags set.
func sdkVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "0.0.0"
	}
	version := info.Main.Version
	if info.Main.Path != modulePath {
		for _, dependency := range info.Deps {
			if dependency.Path == modulePath {
				version = dependency.Version
				break
			}
		}
	}
	version = strings.TrimPrefix(version, "v")
	if version == "" || version == "(devel)" {
		return "0.0.0"
	}
	return version
}
