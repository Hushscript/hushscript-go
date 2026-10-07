package hushscript

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// createWith answers the rig's create with a direct-upload plan made of the given part fields.
func createWith(status int, partFields string) func(*seen) (int, string, bool) {
	return func(s *seen) (int, string, bool) {
		if s.route != "create" {
			return 0, "", false
		}
		return status, `{"job_id":"job_1","state":"uploading","balance_seconds":1,"held_seconds":1,"funding":"personal","limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":1,"used_today_seconds":0},"upload":{` +
			partFields + `,"part_urls":[],"next_part_number":1}}`, true
	}
}

// R52: a part size or part count that is not a positive whole number is no plan at all, so the answer is
// unusable (bad_response, with the status the API sent); bad_part_plan stays for a plan that disagrees with itself.
func TestAPartPlanWithoutPositiveWholeNumbersIsBadResponseWithTheKeyAndTheResumeToken(t *testing.T) {
	for name, fields := range map[string]string{
		"part size zero":       `"part_size_bytes":0,"total_parts":4`,
		"part size negative":   `"part_size_bytes":-161000,"total_parts":4`,
		"total parts zero":     `"part_size_bytes":161000,"total_parts":0`,
		"total parts negative": `"part_size_bytes":161000,"total_parts":-4`,
		"both zero":            `"part_size_bytes":0,"total_parts":0`,
	} {
		for _, status := range []int{200, 201} {
			t.Run(fmt.Sprintf("%s %d", name, status), func(t *testing.T) {
				r := newRig(t)
				r.api.setHook(createWith(status, fields))
				_, err := r.transcribe(nil)
				e := wantCode(t, err, KindAPI, "bad_response")
				if e.Status != status || e.IdempotencyKey == "" || e.ResumeToken == "" {
					t.Errorf("status %d, key %q, token %q", e.Status, e.IdempotencyKey, e.ResumeToken)
				}
				if r.api.count("r2")+r.api.count("part")+r.api.count("complete") != 0 {
					t.Error("parts were sent for a plan that is no plan")
				}
			})
		}
	}
}

func TestAPartPlanThatIsNotWholeNumbersIsBadResponseWithTheKey(t *testing.T) {
	for name, fields := range map[string]string{
		"fractional part size":   `"part_size_bytes":1.5,"total_parts":4`,
		"fractional total":       `"part_size_bytes":161000,"total_parts":2.5`,
		"part size as text":      `"part_size_bytes":"161000","total_parts":4`,
		"part size beyond int64": `"part_size_bytes":99999999999999999999,"total_parts":4`,
		"part size NaN":          `"part_size_bytes":NaN,"total_parts":4`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.api.setHook(createWith(200, fields))
			_, err := r.transcribe(nil)
			e := wantCode(t, err, KindAPI, "bad_response")
			if e.Status != 200 || e.IdempotencyKey == "" {
				t.Errorf("status %d, key %q", e.Status, e.IdempotencyKey)
			}
			if r.api.count("r2")+r.api.count("part") != 0 {
				t.Error("parts were sent for a plan that is no plan")
			}
		})
	}
}

func TestResumeRefusesAnUnusablePartPlanAsBadResponse(t *testing.T) {
	r := newRig(t)
	token := encodeResumeToken(resumeState{JobID: "job_1", IdempotencyKey: "k-0123456789abcdef", Size: int64(len(r.wav)), DurationSeconds: 20})
	r.api.setHook(func(s *seen) (int, string, bool) {
		switch s.route {
		case "job":
			return 200, jobJSON("job_1", "uploading", ""), true
		case "create":
			return createWith(200, `"part_size_bytes":0,"total_parts":4`)(s)
		}
		return 0, "", false
	})
	_, err := r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav))})
	e := wantCode(t, err, KindAPI, "bad_response")
	if e.ResumeToken != token || e.IdempotencyKey != "k-0123456789abcdef" {
		t.Errorf("key %q, token %q", e.IdempotencyKey, e.ResumeToken)
	}
}

// R53: an id that is empty, "." or ".." (or not UTF-8) cannot be a path segment: invalid_option before any request.
func TestAnIdThatCannotBeAPathSegmentIsInvalidOptionBeforeAnyRequest(t *testing.T) {
	badIDs := map[string]string{"empty": "", "dot": ".", "dot dot": "..", "invalid UTF-8": "tr_\xff", "truncated rune": "a\xc3"}
	for name, id := range badIDs {
		calls := map[string]func(c *Client) error{
			"Jobs.Get":        func(c *Client) error { _, err := c.Jobs.Get(context.Background(), id); return err },
			"Jobs.Wait":       func(c *Client) error { _, err := c.Jobs.Wait(context.Background(), id, nil); return err },
			"Transcripts.Get": func(c *Client) error { _, err := c.Transcripts.Get(context.Background(), id); return err },
		}
		for call, run := range calls {
			t.Run(call+" "+name, func(t *testing.T) {
				api := newRouteServer(t, func(recorded, int) step { return step{status: 200, body: `{}`} })
				e := wantCode(t, run(api.client(t)), KindPreflight, "invalid_option")
				if !e.Kind.IsPreflight() || e.Status != 0 || len(api.all()) != 0 {
					t.Errorf("kind %s, status %d, %d requests", e.Kind, e.Status, len(api.all()))
				}
			})
		}
		t.Run("AgentClaim "+name, func(t *testing.T) {
			api := newRouteServer(t, func(recorded, int) step { return step{status: 200, body: `{}`} })
			_, err := AgentClaim(context.Background(), id, "cs", tokenless(api)...)
			_ = wantCode(t, err, KindPreflight, "invalid_option")
			if len(api.all()) != 0 {
				t.Errorf("%d requests", len(api.all()))
			}
		})
	}
}

// Everything else that is a legal id goes out percent-encoded as one segment.
func TestAnIdIsPercentEncodedAsOnePathSegment(t *testing.T) {
	for id, want := range map[string]string{
		"a/b": "a%2Fb", "x y?z#w": "x%20y%3Fz%23w", "%2e%2e": "%252e%252e", "..a": "..a", "a..": "a..", "ключ": "%D0%BA%D0%BB%D1%8E%D1%87", "a\x00b": "a%00b",
	} {
		t.Run(id, func(t *testing.T) {
			api := newRouteServer(t, func(recorded, int) step { return step{status: 404, body: jsonError("not_found", 4040)} })
			_, _ = api.client(t).Jobs.Get(context.Background(), id)
			_, _ = api.client(t).Transcripts.Get(context.Background(), id)
			_, _ = AgentClaim(context.Background(), id, "cs", tokenless(api)...)
			wantPaths := []string{"/v1/jobs/" + want, "/v1/transcripts/" + want, "/v1/agent/accounts/" + want + "/claim"}
			sent := api.all()
			if len(sent) != len(wantPaths) {
				t.Fatalf("%d requests, want %d", len(sent), len(wantPaths))
			}
			for i, path := range wantPaths {
				if sent[i].path != path {
					t.Errorf("request %d went to %q, want %q", i, sent[i].path, path)
				}
			}
		})
	}
}

func TestAResumeTokenOrAnAnswerNamingAnUnusableIdIsRefused(t *testing.T) {
	r := newRig(t)
	for _, id := range []string{".", "..", ""} {
		token := encodeResumeToken(resumeState{JobID: id, IdempotencyKey: "k-0123456789abcdef", Size: 10, DurationSeconds: 20})
		_, err := r.client.Resume(context.Background(), token, nil)
		_ = wantCode(t, err, KindConfiguration, "config_invalid")
	}
	if n := len(r.api.log); n != 0 {
		t.Fatalf("%d requests for refused tokens", n)
	}
	for name, hook := range map[string]func(s *seen) (int, string, bool){
		"job id in a create": func(s *seen) (int, string, bool) {
			if s.route != "create" {
				return 0, "", false
			}
			return 200, `{"job_id":"..","state":"uploading"}`, true
		},
		"transcript id in a job": func(s *seen) (int, string, bool) {
			if s.route != "job" {
				return 0, "", false
			}
			return 200, strings.Replace(jobJSON("job_1", "done", ""), `"transcript_id":null`, `"transcript_id":".."`, 1), true
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.api.setHook(hook)
			_, err := r.transcribe(nil)
			_ = wantCode(t, err, KindAPI, "bad_response")
			if r.api.count("transcript") != 0 {
				t.Error("a transcript was requested at an unusable id")
			}
		})
	}
}

// R53: an error body that does not parse, however deeply nested, is handled as non-JSON.
func TestADeeplyNestedBodyIsNonJSONAndNeverPanics(t *testing.T) {
	const depth = 200_000
	bodies := map[string]string{
		"deep array":         strings.Repeat("[", depth) + strings.Repeat("]", depth),
		"deep object":        `{"error":"unauthorized","code":1,` + strings.Repeat(`"a":{`, depth) + strings.Repeat("}", depth) + `}`,
		"deep array in key":  `{"error":"unauthorized","code":1,"d":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`,
		"unterminated depth": strings.Repeat("[", depth),
	}
	for name, body := range bodies {
		for _, status := range []int{400, 401, 403, 404, 409, 422, 500, 502} {
			t.Run(fmt.Sprintf("%s %d", name, status), func(t *testing.T) {
				e := fromResponse(status, nil, body, http.MethodGet, "/v1/jobs/j1")
				if e.Code != fmt.Sprintf("http_%d", status) || e.Status != status || e.NumericCode != 0 || len(e.Details) != 0 {
					t.Errorf("code %q, status %d, numeric %d, details %d", e.Code, e.Status, e.NumericCode, len(e.Details))
				}
			})
		}
	}
	for name, body := range bodies {
		t.Run("through the client "+name, func(t *testing.T) {
			api := newListAPI(t, func(string, url.Values) (int, string) { return 404, body })
			_, err := api.client(t).Jobs.Get(context.Background(), "j1")
			if e := asError(t, err); e.Code != "http_404" || e.Kind != KindNotFound {
				t.Errorf("error %+v", e)
			}
			for _, status := range []int{200, 201} {
				ok := newListAPI(t, func(string, url.Values) (int, string) { return status, body })
				_, err = ok.client(t).Jobs.Get(context.Background(), "j1")
				if e := wantCode(t, err, KindAPI, "bad_response"); e.Status != status {
					t.Errorf("status %d, want %d", e.Status, status)
				}
				_, err = ok.client(t).Account(context.Background())
				_ = wantCode(t, err, KindAPI, "bad_response")
			}
		})
	}
	t.Run("nested below the decoder limit keeps its slug", func(t *testing.T) {
		body := `{"error":"unauthorized","code":1,"d":` + strings.Repeat("[", 9000) + strings.Repeat("]", 9000) + `}`
		if e := fromResponse(401, nil, body, http.MethodGet, "/v1/jobs/j1"); e.Code != "unauthorized" || e.Kind != KindAuthentication {
			t.Errorf("error %+v", e)
		}
	})
}

// R54: net/http replays a request only when it is replayable (an idempotent method, or an Idempotency-Key
// header); the SDK sets no such header, so a non-idempotent request that was sent is never sent twice.
// The server below answers the first request of each connection and closes the connection after reading
// the whole of the second one, which is how a keep-alive connection dies after the request was sent.
type killServer struct {
	ln       net.Listener
	mu       sync.Mutex
	requests []killed
	conns    []net.Conn
}

type killed struct {
	method, path string
	header       http.Header
	seq          int // 1 for the first request on its connection
}

func newKillServer(t *testing.T) *killServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &killServer{ln: ln}
	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, conn := range s.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			go s.serve(conn)
		}
	}()
	return s
}

func (s *killServer) URL() string { return "http://" + s.ln.Addr().String() }

func (s *killServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for seq := 1; ; seq++ {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		s.mu.Lock()
		s.requests = append(s.requests, killed{req.Method, req.URL.Path, req.Header.Clone(), seq})
		s.mu.Unlock()
		if seq >= 2 {
			return // the whole request was read and nothing is answered: the connection dies
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}")
	}
}

func (s *killServer) sent(method, path string) []killed {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []killed
	for _, req := range s.requests {
		if req.method == method && req.path == path {
			out = append(out, req)
		}
	}
	return out
}

// warmUp leaves one idle keep-alive connection to the server in the transport's pool; the next request
// to the server must be the second one on it.
func warmUp(t *testing.T, c *Client, s *killServer) {
	t.Helper()
	if _, err := do(t, c, http.MethodGet, "/v1/warm", ""); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if got := s.sent(http.MethodGet, "/v1/warm"); len(got) != 1 || got[0].seq != 1 {
		t.Fatalf("warm-up requests = %+v", got)
	}
}

func TestANonIdempotentRequestThatWasSentIsNeverReplayedOnAReusedConnectionThatDies(t *testing.T) {
	const body = `{"size_bytes":100,"duration_seconds":20}`
	cases := []struct {
		name, method, path, body string
		kind                     ErrorKind
		code                     string
		run                      func(t *testing.T, c *Client, base string) error
	}{
		{
			name: "claim", method: http.MethodPost, path: "/v1/agent/accounts/sg_1/claim", kind: KindOutcomeUnknown, code: "outcome_unknown",
			run: func(_ *testing.T, _ *Client, base string) error {
				_, err := AgentClaim(context.Background(), "sg_1", "cs", WithBaseURL(base), WithClock(newFakeClock()))
				return err
			},
		},
		{
			name: "rotate", method: http.MethodPost, path: "/v1/agent/credentials/rotate", kind: KindOutcomeUnknown, code: "outcome_unknown",
			run: func(_ *testing.T, c *Client, _ string) error {
				_, err := c.Agent.RotateToken(context.Background())
				return err
			},
		},
		{
			name: "org mutation", method: http.MethodPost, path: "/v1/org/custody", body: `{"custody":"firm"}`, kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ string) error {
				_, err := do(t, c, http.MethodPost, "/v1/org/custody", `{"custody":"firm"}`)
				return err
			},
		},
		{
			name: "org member patch", method: http.MethodPatch, path: "/v1/org/members/u_1", body: `{"role":"admin"}`, kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ string) error {
				_, err := do(t, c, http.MethodPatch, "/v1/org/members/u_1", `{"role":"admin"}`)
				return err
			},
		},
		{
			name: "unkeyed create", method: http.MethodPost, path: "/v1/uploads", body: body, kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ string) error {
				_, err := do(t, c, http.MethodPost, "/v1/uploads", body)
				return err
			},
		},
		{
			name: "purchase", method: http.MethodPost, path: "/v1/billing/purchase", kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ string) error {
				_, err := do(t, c, http.MethodPost, "/v1/billing/purchase", `{"pack":"45min","idempotency_key":"k-0123456789abcdef"}`)
				return err
			},
		},
		{
			name: "delete", method: http.MethodDelete, path: "/v1/transcripts/tr_1", kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ string) error {
				_, err := do(t, c, http.MethodDelete, "/v1/transcripts/tr_1", "")
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newKillServer(t)
			// The purchase class retries a lost answer once on its own, with its key; switch that off so
			// only the transport could send it twice. Every other class gets its default budget.
			opts := []Option{}
			if tc.name == "purchase" {
				opts = append(opts, WithMaxRetries(0))
			}
			c := newTestClient(t, srv.URL(), newFakeClock(), opts...)
			warmUp(t, c, srv)
			err := tc.run(t, c, srv.URL())
			e := wantCode(t, err, tc.kind, tc.code)
			if e.Status != 0 {
				t.Errorf("status %d on a request that got no answer", e.Status)
			}
			sent := srv.sent(tc.method, tc.path)
			if len(sent) != 1 || sent[0].seq != 2 {
				t.Fatalf("the request reached the server %d times (%+v), want once, on the reused connection", len(sent), sent)
			}
			for name := range sent[0].header {
				if strings.EqualFold(name, "Idempotency-Key") || strings.EqualFold(name, "X-Idempotency-Key") {
					t.Errorf("the SDK set %s on a request that must never be replayed", name)
				}
			}
		})
	}
}

// The control that makes the test above mean something: a GET is replayable, so net/http does resend it
// on a fresh connection after the reused one died, and the server sees it twice.
func TestAGetOnAReusedConnectionThatDiesIsReplayedByNetHTTP(t *testing.T) {
	srv := newKillServer(t)
	c := newTestClient(t, srv.URL(), newFakeClock(), WithMaxRetries(0))
	warmUp(t, c, srv)
	if _, err := do(t, c, http.MethodGet, "/v1/account", ""); err != nil {
		t.Fatalf("the replay should have succeeded: %v", err)
	}
	sent := srv.sent(http.MethodGet, "/v1/account")
	if len(sent) != 2 || sent[0].seq != 2 || sent[1].seq != 1 {
		t.Errorf("GET requests = %+v, want the second on the dead connection and a replay on a new one", sent)
	}
}

// Item 6: the request net/http keeps on the response is not ours to change.
func TestTheRequestOnTheResponseIsACopyWithoutTheToken(t *testing.T) {
	var sentByTransport *http.Request
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sentByTransport = req
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Body: io.NopCloser(strings.NewReader(`{}`)), Request: req,
		}, nil
	})
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(&http.Client{Transport: transport}))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.core.cfg.baseURL+"/v1/account", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.core.doer.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if sentByTransport.Header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("the request the transport holds lost its header: %q", sentByTransport.Header.Get("Authorization"))
	}
	if resp.Request == sentByTransport || resp.Request.Header.Get("Authorization") != "" {
		t.Errorf("the response still holds the token or the transport's own request: %q", resp.Request.Header.Get("Authorization"))
	}
	if resp.Request.URL.Path != "/v1/account" || resp.Request.Method != http.MethodGet {
		t.Errorf("the copy is not the request: %s %s", resp.Request.Method, resp.Request.URL)
	}
}

// Item 5: a nil *Error or *Client behind an interface or a pointer is logged, not a panic.
func TestLoggingATypedNilErrorOrClientDoesNotPanic(t *testing.T) {
	var nilError *Error
	var nilClient *Client
	var asErrorValue error = nilError
	if got := nilError.LogValue().String(); got != "<nil>" {
		t.Errorf("nil *Error logs %q", got)
	}
	if got := nilClient.LogValue().String(); got != "<nil>" {
		t.Errorf("nil *Client logs %q", got)
	}
	for name, handler := range map[string]func(*bytes.Buffer) slog.Handler{
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			var buffer bytes.Buffer
			logger := slog.New(handler(&buffer))
			logger.Error("m", "err", nilError, "client", nilClient, slog.Any("iface", asErrorValue), slog.Group("g", slog.Any("err", nilError)))
			if out := buffer.String(); strings.Contains(out, "panicked") || strings.Contains(out, "goroutine") || !strings.Contains(out, "<nil>") {
				t.Errorf("log = %s", out)
			}
		})
	}
	var buffer bytes.Buffer
	live := newSDKError(KindNetwork, "network_error", "boom", errors.New("x"), newRedactor(testToken), hintInputs{})
	slog.New(slog.NewTextHandler(&buffer, nil)).Error("m", "err", live)
	if !strings.Contains(buffer.String(), "hushscript: network_error") {
		t.Errorf("a live error logs %s", buffer.String())
	}
}

// Item 7: a server that echoes only the secret segment of the token gives nothing away.
func TestRedactsAnEchoOfOnlyTheSecretSegment(t *testing.T) {
	secretSegment := testSecret[strings.LastIndex(testSecret, ".")+1:]
	idAndSecret := testSecret
	r := newRedactor(testToken)
	for name, text := range map[string]string{
		"secret segment":    "denied for " + secretSegment + ".",
		"id and secret":     "denied for " + idAndSecret + ".",
		"secret in a quote": `{"detail":"` + secretSegment + `"}`,
	} {
		if out := r.apply(text); strings.Contains(out, secretSegment) {
			t.Errorf("%s survived: %q", name, out)
		}
	}
	rotated := newTokenBox(testToken)
	rotated.swap(newTokenText)
	past := rotated.redactor()
	if out := past.apply("old " + secretSegment + " new " + strings.Repeat("b", 64)); strings.Contains(out, secretSegment) || strings.Contains(out, strings.Repeat("b", 64)) {
		t.Errorf("a replaced or current token's secret segment survived: %q", out)
	}
	short := newRedactor("hsr1:eu:pat.aaaa.bbbb")
	if got := short.apply("keep bbbb and aaaa"); got != "keep bbbb and aaaa" {
		t.Errorf("a segment below the length guard was redacted everywhere: %q", got)
	}
}
