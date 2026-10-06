package hushscript

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/Hushscript/hushscript-go/generated"
)

const modulePath = "github.com/Hushscript/hushscript-go"

// Client is the entry point of the SDK. It is safe for concurrent use.
type Client struct {
	core *core
}

// core holds everything a Client needs. Client points at it, so printing a Client value shows an
// address and never the configuration.
type core struct {
	cfg  config
	doer *doer
	api  *generated.ClientWithResponses
}

// New builds a Client from the options, the environment and the defaults (see the With options).
// It returns a *Error of kind KindConfiguration when the configuration is unusable.
func New(opts ...Option) (*Client, error) {
	cfg, err := resolve(opts)
	if err != nil {
		return nil, err
	}
	d := newDoer(cfg)
	api, err := generated.NewClientWithResponses(cfg.baseURL, generated.WithHTTPClient(d))
	if err != nil {
		return nil, newSDKError(KindConfiguration, "config_invalid", "The generated client could not be built.", err, cfg.redactor())
	}
	return &Client{core: &core{cfg: cfg, doer: d, api: api}}, nil
}

// API returns the generated client for every /v1 operation, sharing the SDK's retries, timeouts and
// error handling.
func (c Client) API() *generated.ClientWithResponses {
	return c.core.api
}

// String describes the client without its token.
func (c Client) String() string {
	cfg := c.core.cfg
	return fmt.Sprintf("hushscript.Client{baseURL: %s, timeout: %s, maxRetries: %d, token: %s}",
		cfg.baseURL, cfg.timeout, cfg.maxRetries, redactedToken)
}

// GoString is String, so %#v never shows the token either.
func (c Client) GoString() string { return c.String() }

// Format covers every fmt verb, including %d and %x, which would otherwise print the struct fields.
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// userAgent is "hushscript-go/<version> go/<go version>" (spec section 3).
func userAgent() string {
	return fmt.Sprintf("hushscript-go/%s go/%s", sdkVersion(), strings.TrimPrefix(runtime.Version(), "go"))
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
