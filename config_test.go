package hushscript

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestRefusesConstructionWithoutATokenAndSaysWhereToGetOne(t *testing.T) {
	t.Setenv("HUSHSCRIPT_TOKEN", "")
	_, err := New()
	apiErr := asError(t, err)
	if apiErr.Kind != KindConfiguration || apiErr.Code != "config_invalid" {
		t.Fatalf("kind %v code %q", apiErr.Kind, apiErr.Code)
	}
	if !strings.Contains(apiErr.Message, "HUSHSCRIPT_TOKEN") {
		t.Errorf("message %q does not name the variable", apiErr.Message)
	}
	wantHint := "Set HUSHSCRIPT_TOKEN or pass a token. Create one in the Hushscript app: " +
		"Account → MCP (app.hushscript.com/account/mcp)."
	if apiErr.Hint != wantHint {
		t.Errorf("hint = %q, want the spec section 3 text", apiErr.Hint)
	}
	if apiErr.Status != 0 || apiErr.NumericCode != 0 {
		t.Errorf("an SDK-side error carries no status or numeric code: %+v", apiErr)
	}
}

func TestReadsTheTokenFromTheEnvironmentAndLetsTheOptionWin(t *testing.T) {
	t.Setenv("HUSHSCRIPT_TOKEN", testToken)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.core.cfg.token.reveal(); got != testToken {
		t.Errorf("token from environment = %q", got)
	}
	other := "hsr1:us:pat.aa.bb"
	c, err = New(WithToken(other))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.core.cfg.token.reveal(); got != other {
		t.Errorf("token from option = %q", got)
	}
}

func TestRefusesATokenThatDoesNotStartWithHsr(t *testing.T) {
	_, err := New(WithToken("sk-not-ours"))
	apiErr := asError(t, err)
	if apiErr.Kind != KindConfiguration || !strings.Contains(apiErr.Message, "This doesn't look like a Hushscript token") {
		t.Errorf("error = %v", apiErr)
	}
	if strings.Contains(apiErr.Error(), "sk-not-ours") {
		t.Errorf("the rejected token leaked into %q", apiErr.Error())
	}
}

func TestChecksOnlyThePrefixOfTheToken(t *testing.T) {
	if _, err := New(WithToken("hsr-anything-the-format-may-change")); err != nil {
		t.Errorf("a token with the hsr prefix was refused: %v", err)
	}
}

func TestAppliesTheDocumentedDefaults(t *testing.T) {
	t.Setenv("HUSHSCRIPT_BASE_URL", "")
	t.Setenv("HUSHSCRIPT_LOG", "")
	c, err := New(WithToken(testToken))
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.core.cfg
	if cfg.baseURL != "https://api.hushscript.com" {
		t.Errorf("baseURL = %q", cfg.baseURL)
	}
	if cfg.timeout != 60*time.Second {
		t.Errorf("timeout = %v", cfg.timeout)
	}
	if cfg.maxRetries != 3 {
		t.Errorf("maxRetries = %d", cfg.maxRetries)
	}
	if cfg.logger != nil {
		t.Error("logging is off by default")
	}
	if c.API() == nil {
		t.Error("API() returned nil")
	}
}

func TestAcceptsOnlyHTTPSOrLoopbackHTTPBaseURLs(t *testing.T) {
	good := []string{
		"https://api.hushscript.com", "https://api.hushscript.com/", "http://127.0.0.1:4010",
		"http://localhost:4010", "http://[::1]:4010", "https://example.test:8443",
	}
	for _, base := range good {
		c, err := New(WithToken(testToken), WithBaseURL(base))
		if err != nil {
			t.Errorf("%s refused: %v", base, err)
			continue
		}
		if strings.HasSuffix(c.core.cfg.baseURL, "/") {
			t.Errorf("%s kept its trailing slash: %q", base, c.core.cfg.baseURL)
		}
	}
	bad := []string{
		"http://api.hushscript.com", "ftp://127.0.0.1", "api.hushscript.com", "", "http://", "https://", "://x", "http://127.0.0.1.evil.test",
		"https://api.hushscript.com/v1", "https://api.hushscript.com?x=1", "https://api.hushscript.com#frag", "https://u:p@api.hushscript.com",
	}
	for _, base := range bad {
		_, err := New(WithToken(testToken), WithBaseURL(base))
		if err == nil {
			t.Errorf("%q was accepted", base)
			continue
		}
		if asError(t, err).Kind != KindConfiguration {
			t.Errorf("%q: wrong kind", base)
		}
	}
}

func TestReadsTheBaseURLFromTheEnvironment(t *testing.T) {
	t.Setenv("HUSHSCRIPT_BASE_URL", "http://127.0.0.1:4010")
	c, err := New(WithToken(testToken))
	if err != nil {
		t.Fatal(err)
	}
	if c.core.cfg.baseURL != "http://127.0.0.1:4010" {
		t.Errorf("baseURL = %q", c.core.cfg.baseURL)
	}
	t.Setenv("HUSHSCRIPT_BASE_URL", "http://evil.test")
	if _, err := New(WithToken(testToken)); err == nil {
		t.Error("a bad base URL in the environment was accepted")
	}
}

func TestRejectsNonsenseOptionValues(t *testing.T) {
	for name, opt := range map[string]Option{
		"zero timeout":     WithTimeout(0),
		"negative timeout": WithTimeout(-time.Second),
		"negative retries": WithMaxRetries(-1),
		"nil clock":        WithClock(nil),
		"nil http client":  WithHTTPClient(nil),
	} {
		if _, err := New(WithToken(testToken), opt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	c, err := New(WithToken(testToken), WithTimeout(5*time.Second), WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	if c.core.cfg.timeout != 5*time.Second || c.core.cfg.maxRetries != 0 {
		t.Errorf("options not applied: %+v", c.core.cfg.timeout)
	}
}

func TestLoggingIsOffUnlessAskedFor(t *testing.T) {
	t.Setenv("HUSHSCRIPT_LOG", "debug")
	c, err := New(WithToken(testToken))
	if err != nil {
		t.Fatal(err)
	}
	if c.core.cfg.logger == nil {
		t.Error("HUSHSCRIPT_LOG=debug did not enable logging")
	}
	t.Setenv("HUSHSCRIPT_LOG", "off")
	c, err = New(WithToken(testToken))
	if err != nil || c.core.cfg.logger != nil {
		t.Errorf("HUSHSCRIPT_LOG=off: logger %v, err %v", c.core.cfg.logger, err)
	}
	t.Setenv("HUSHSCRIPT_LOG", "verbose")
	if _, verboseErr := New(WithToken(testToken)); verboseErr == nil {
		t.Error("an unknown log level was accepted")
	}
	t.Setenv("HUSHSCRIPT_LOG", "debug")
	c, err = New(WithToken(testToken), WithLogLevel("off"))
	if err != nil || c.core.cfg.logger != nil {
		t.Errorf("WithLogLevel(off) should beat HUSHSCRIPT_LOG=debug: logger %v, err %v", c.core.cfg.logger, err)
	}
	t.Setenv("HUSHSCRIPT_LOG", "off")
	c, err = New(WithToken(testToken), WithLogLevel("debug"))
	if err != nil || c.core.cfg.logger == nil {
		t.Errorf("WithLogLevel(debug) should beat HUSHSCRIPT_LOG=off: err %v", err)
	}
	if _, levelErr := New(WithToken(testToken), WithLogLevel("info")); levelErr == nil {
		t.Error("an unknown level was accepted")
	}
	own := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	t.Setenv("HUSHSCRIPT_LOG", "")
	c, err = New(WithToken(testToken), WithLogger(own))
	if err != nil || c.core.cfg.logger != own {
		t.Errorf("WithLogger not applied: %v", err)
	}
}

func TestNeverPrintsTheToken(t *testing.T) {
	c, err := New(WithToken(testToken))
	if err != nil {
		t.Fatal(err)
	}
	cfg := c.core.cfg
	values := map[string]any{"client": c, "client value": *c, "config": cfg, "config pointer": &cfg}
	for name, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			out := fmt.Sprintf(verb, value)
			if strings.Contains(out, testSecret) || strings.Contains(out, testToken) || strings.Contains(out, "pat.") {
				t.Errorf("%s with %s printed the token: %s", name, verb, out)
			}
		}
	}
	both := fmt.Sprintf("%v %+v", c, c)
	if strings.Contains(both, testSecret) {
		t.Error("the combined v and plus-v forms printed the token")
	}
	if !strings.Contains(c.String(), "hsr…[redacted]") {
		t.Errorf("the string form should show the redaction marker: %v", c)
	}
	if !strings.Contains(c.GoString(), "hsr…[redacted]") {
		t.Error("GoString should show the redaction marker")
	}
	if got := cfg.token.String() + cfg.token.GoString() + cfg.token.String(); strings.Contains(got, testSecret) {
		t.Errorf("the secret type printed itself: %s", got)
	}
}

func TestNeverLogsTheToken(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", "token", newSecret(testToken))
	if strings.Contains(buf.String(), testSecret) {
		t.Errorf("slog printed the token: %s", buf.String())
	}
}

func TestSendsAnIdentifyingUserAgent(t *testing.T) {
	if !regexp.MustCompile(`^hushscript-go/\S+ go/\d+\.\d+(\.\d+)?$`).MatchString(userAgent()) {
		t.Errorf("user agent = %q", userAgent())
	}
}

func TestSystemClockSleepsAndHonorsCancellation(t *testing.T) {
	var clock Clock = systemClock{}
	before := clock.Now()
	if err := clock.Sleep(context.Background(), 5*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if clock.Now().Sub(before) < 5*time.Millisecond {
		t.Error("Sleep returned early")
	}
	if err := clock.Sleep(context.Background(), 0); err != nil {
		t.Errorf("a zero sleep failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.Sleep(ctx, time.Hour); err == nil {
		t.Error("a cancelled sleep returned nil")
	}
}

func TestRejectsATokenWithCharactersOutsidePrintableASCIIWithoutEchoingIt(t *testing.T) {
	for _, token := range []string{"hsr1 with space", "hsr1\nnewline", "hsr1\x00nul", "hsr1:eu:pat.…", "hsr\x7f", "hsr1\ttab"} {
		_, err := New(WithToken(token))
		apiErr := asError(t, err)
		if apiErr.Kind != KindConfiguration {
			t.Errorf("%q: kind %v", token, apiErr.Kind)
		}
		if strings.Contains(apiErr.Error(), strings.TrimPrefix(token, "hsr")) && len(token) > 5 {
			t.Errorf("%q was echoed in %q", token, apiErr.Error())
		}
	}
}
