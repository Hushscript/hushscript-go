package hushscript

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	signupRoute = "POST /v1/agent/accounts"
	claimRoute  = "POST /v1/agent/accounts/sg_1/claim"
	rotateRoute = "POST /v1/agent/credentials/rotate"

	claimHint  = "The claim may have succeeded. Retrying will fail with agent_claim_invalid. If you have no token, contact support with the signup id."
	rotateHint = "The old token may already be revoked. Try a request with it; if it fails, the new token was lost and the account needs support."

	newTokenText = "hsr1:eu:pat.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" //nolint:gosec // a made-up token
)

func signupAnswer() step {
	return ok(`{"signup_id":"hsr1:eu:abc123","claim_secret":"cs_secret","checkout_url":"https://pay.example/c/1","amount":199,"currency":"usd","expires_at":1760001800,"pack":"45min"}`)
}

func claimAnswer() step {
	return ok(`{"pat":"` + newTokenText + `","user_id":"u_1","scopes":["transcribe","billing"],"balance_seconds":2700}`)
}

func rotateAnswer(expires string) step {
	return ok(`{"pat":"` + newTokenText + `","pat_id":"pat_2","scopes":["transcribe"],"expires_at":` + expires + `}`)
}

func tokenless(srv *routeServer) []Option {
	return []Option{WithBaseURL(srv.URL), WithClock(newFakeClock())}
}

func request() AgentSignupRequest {
	return AgentSignupRequest{
		Email: "agent@example.com", Pack: "45min", AcceptTerms: "2026-10-01",
		Agent: AgentInfo{Name: "My agent", Platform: "custom"},
	}
}

func TestSignupNeedsAnAcceptedTermsVersionAndSendsNothingWithoutOne(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step { return signupAnswer() })
	req := request()
	req.AcceptTerms = ""
	_, err := AgentSignup(context.Background(), req, tokenless(srv)...)
	e := wantCode(t, err, KindValidation, "accept_terms_required")
	if e.Status != 0 || e.NumericCode != 0 || len(srv.all()) != 0 || !strings.Contains(e.Message, "acceptTerms") && !strings.Contains(e.Message, "AcceptTerms") {
		t.Errorf("status %d, %d requests, message %q", e.Status, len(srv.all()), e.Message)
	}
}

func TestSignupSendsTheRequestWithoutAnyTokenEvenWhenOneIsInTheEnvironment(t *testing.T) {
	t.Setenv("HUSHSCRIPT_TOKEN", testToken)
	srv := newRouteServer(t, func(recorded, int) step { return signupAnswer() })
	req := request()
	req.Agent.ContactURL = "https://agent.example"
	req.DataRegion = "eu"
	req.DryRun = true
	got, err := AgentSignup(context.Background(), req, append(tokenless(srv), WithToken(testToken))...)
	if err != nil {
		t.Fatal(err)
	}
	if got.SignupID != "hsr1:eu:abc123" || got.ClaimSecret != "cs_secret" || got.CheckoutURL != "https://pay.example/c/1" ||
		got.Price != (Money{199, "usd", "$1.99"}) || got.ExpiresAt.Unix() != 1760001800 || got.ExpiresAt.Location() != time.UTC {
		t.Errorf("result = %+v", got)
	}
	sent := srv.to(signupRoute)
	if len(sent) != 1 || sent[0].header.Get("Authorization") != "" {
		t.Fatalf("requests = %v", srv.all())
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(sent[0].body), &body); err != nil {
		t.Fatal(err)
	}
	agent, _ := body["agent"].(map[string]any)
	if body["contact_email"] != "agent@example.com" || body["pack_id"] != "45min" || body["accept_terms_version"] != "2026-10-01" ||
		body["data_region"] != "eu" || body["dry_run"] != true || agent["name"] != "My agent" || agent["platform"] != "custom" || agent["contact_url"] != "https://agent.example" {
		t.Errorf("body = %v", body)
	}
	if !strings.HasPrefix(sent[0].header.Get("User-Agent"), "hushscript-go/") {
		t.Errorf("User-Agent = %q", sent[0].header.Get("User-Agent"))
	}
}

func TestAStaleTermsVersionHintQuotesTheCurrentOne(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step {
		return step{status: 422, body: `{"error":"policy_version_stale","code":1043,"details":{"current_version":"2026-10-01"}}`}
	})
	_, err := AgentSignup(context.Background(), request(), tokenless(srv)...)
	e := wantCode(t, err, KindValidation, "policy_version_stale")
	if e.Hint != "Accept the current terms version: 2026-10-01." {
		t.Errorf("hint = %q", e.Hint)
	}
}

func TestSignupIsNeverRetriedAndALostAnswerIsOutcomeUnknownWithoutAHint(t *testing.T) {
	for name, answer := range map[string]step{"drop": {drop: true}, "503": {status: 503, body: "x"}} {
		srv := newRouteServer(t, func(recorded, int) step { return answer })
		_, err := AgentSignup(context.Background(), request(), tokenless(srv)...)
		e := asError(t, err)
		if len(srv.to(signupRoute)) != 1 {
			t.Errorf("%s: %d requests", name, len(srv.to(signupRoute)))
		}
		if name == "drop" && (e.Kind != KindOutcomeUnknown || e.Code != "outcome_unknown" || e.Status != 0 || e.Hint != "" || e.Unwrap() == nil) {
			t.Errorf("drop: %+v", e)
		}
		if name == "503" && (e.Kind != KindAPI || e.Code != "http_503" || e.Status != 503) {
			t.Errorf("503: %+v", e)
		}
	}
	for name, body := range map[string]string{"empty": `{}`, "no secret": `{"signup_id":"s","checkout_url":"u","amount":1,"currency":"usd","expires_at":1}`} { //nolint:gosec // test data
		srv := newRouteServer(t, func(recorded, int) step { return ok(body) })
		_, err := AgentSignup(context.Background(), request(), tokenless(srv)...)
		if e := wantCode(t, err, KindAPI, "bad_response"); e.Status != 200 {
			t.Errorf("%s: %+v", name, e)
		}
	}
}

func TestClaimReturnsTheTokenOnceAndSendsNoAuthorization(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step { return claimAnswer() })
	got, err := AgentClaim(context.Background(), "sg_1", "cs_secret", tokenless(srv)...)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != newTokenText || got.UserID != "u_1" || len(got.Scopes) != 2 || got.Scopes[1] != "billing" ||
		got.Balance.Seconds != 2700 || got.Balance.Minutes != 45 || got.Balance.HeldSeconds != 0 || got.Balance.ExpiresAt != nil || got.Balance.PurchasesRemaining != nil {
		t.Errorf("claim = %+v", got)
	}
	sent := srv.to(claimRoute)
	if len(sent) != 1 || sent[0].body != `{"claim_secret":"cs_secret"}` || sent[0].header.Get("Authorization") != "" {
		t.Errorf("requests = %v", srv.all())
	}
}

func TestAClaimIdWithColonsIsOnePathSegment(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step { return claimAnswer() })
	if _, err := AgentClaim(context.Background(), "hsr1:eu:abc/123", "cs", tokenless(srv)...); err != nil {
		t.Fatal(err)
	}
	if got := nth(t, srv.all(), 0).path; got != "/v1/agent/accounts/hsr1:eu:abc%2F123/claim" {
		t.Errorf("path = %q", got)
	}
}

func TestClaimBeforeTheCheckoutIsPaidIsAConflictWithoutAJobHint(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step {
		return step{status: 409, body: jsonError("wrong_state", 4001)}
	})
	_, err := AgentClaim(context.Background(), "sg_1", "cs", tokenless(srv)...)
	e := wantCode(t, err, KindConflict, "wrong_state")
	if e.Hint != "" || len(srv.to(claimRoute)) != 1 {
		t.Errorf("hint %q, %d requests", e.Hint, len(srv.to(claimRoute)))
	}
	srv = newRouteServer(t, func(recorded, int) step { return step{status: 401, body: jsonError("agent_claim_invalid", 4036)} })
	_, err = AgentClaim(context.Background(), "sg_1", "cs", tokenless(srv)...)
	_ = wantCode(t, err, KindAuthentication, "agent_claim_invalid")
}

func TestALostClaimOrATokenlessClaimIsOutcomeUnknownWithTheClaimHintAndNeverRetried(t *testing.T) {
	for name, answer := range map[string]step{
		"connection lost": {drop: true},
		"no pat":          ok(`{"user_id":"u_1","scopes":[],"balance_seconds":0}`),
		"empty pat":       ok(`{"pat":"","user_id":"u_1","scopes":[]}`),
		"null pat":        ok(`{"pat":null}`),
		"pat not text":    ok(`{"pat":7}`),
		"not JSON":        ok(`<html>`),
		"empty body":      {status: 200},
		"201 with token":  {status: 201, body: `{"pat":"` + newTokenText + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return answer })
			_, err := AgentClaim(context.Background(), "sg_1", "cs", tokenless(srv)...)
			e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
			if e.Hint != claimHint || len(srv.to(claimRoute)) != 1 || e.Status != 0 {
				t.Errorf("hint %q, %d requests, status %d", e.Hint, len(srv.to(claimRoute)), e.Status)
			}
			var cause *Error
			if name != "connection lost" && (!errors.As(e.Unwrap(), &cause) || cause.Code != "bad_response") {
				t.Errorf("cause = %v, want bad_response", e.Unwrap())
			}
			if name == "connection lost" && e.Unwrap() == nil {
				t.Error("the original error must stay reachable")
			}
		})
	}
}

func TestRotateSwapsTheTokenInPlaceForEveryLaterRequest(t *testing.T) {
	srv := newRouteServer(t, func(rec recorded, _ int) step {
		if rec.path == "/v1/agent/credentials/rotate" {
			return rotateAnswer("1790000000")
		}
		return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
	})
	c := srv.client(t)
	got, err := c.Agent.RotateToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != newTokenText || got.TokenID != "pat_2" || len(got.Scopes) != 1 || got.ExpiresAt == nil || got.ExpiresAt.Unix() != 1790000000 {
		t.Errorf("rotated = %+v", got)
	}
	if _, err := c.Balance(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := srv.all()
	if all[0].header.Get("Authorization") != "Bearer "+testToken || all[1].header.Get("Authorization") != "Bearer "+newTokenText {
		t.Errorf("Authorization: rotate %q, next %q", all[0].header.Get("Authorization"), all[1].header.Get("Authorization"))
	}
	if strings.Contains(c.String(), newTokenText) || strings.Contains(c.String(), "bbbbbbbbbbbb") {
		t.Errorf("String() = %s", c)
	}
}

func TestRotateWithNoExpiryReportsNil(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step { return rotateAnswer("null") })
	got, err := srv.client(t).Agent.RotateToken(context.Background())
	if err != nil || got.ExpiresAt != nil {
		t.Errorf("%+v, %v", got, err)
	}
}

// A retry already waiting in its backoff when the token rotates sends the new token (spec 7.4).
func TestARetryQueuedBeforeARotationSendsTheNewToken(t *testing.T) {
	srv := newRouteServer(t, func(rec recorded, nth int) step {
		switch {
		case rec.path == "/v1/agent/credentials/rotate":
			return rotateAnswer("null")
		case nth == 1:
			return step{status: 503, body: "x"}
		}
		return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
	})
	var c *Client
	rotated := false
	clock := &hookClock{fakeClock: newFakeClock(), onSleep: func() {
		if rotated {
			return
		}
		rotated = true
		if _, err := c.Agent.RotateToken(context.Background()); err != nil {
			t.Errorf("rotate: %v", err)
		}
	}}
	c = newTestClient(t, srv.URL, clock)
	if _, err := c.Balance(context.Background()); err != nil {
		t.Fatal(err)
	}
	reads := srv.to("GET /v1/account")
	if len(reads) != 2 || reads[0].header.Get("Authorization") != "Bearer "+testToken || reads[1].header.Get("Authorization") != "Bearer "+newTokenText {
		t.Errorf("reads = %v", reads)
	}
}

type hookClock struct {
	*fakeClock
	onSleep func()
}

func (h *hookClock) Sleep(ctx context.Context, d time.Duration) error {
	h.onSleep()
	return h.fakeClock.Sleep(ctx, d)
}

func TestTheRotatedTokenIsRedactedFromLaterErrors(t *testing.T) {
	srv := newRouteServer(t, func(rec recorded, _ int) step {
		if rec.path == "/v1/agent/credentials/rotate" {
			return rotateAnswer("null")
		}
		return step{status: 401, body: `{"error":"unauthorized","code":2000,"echo":"` + strings.TrimPrefix(rec.header.Get("Authorization"), "Bearer ") + `"}`}
	})
	c := srv.client(t)
	if _, err := c.Agent.RotateToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := c.Balance(context.Background())
	e := asError(t, err)
	for _, surface := range []string{e.Error(), e.Message, e.Hint, fmt.Sprint(e.Details["echo"])} {
		if strings.Contains(surface, newTokenText) || strings.Contains(surface, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
			t.Errorf("a surface leaks the rotated token: %q", surface)
		}
	}
}

func TestALostRotateIsOutcomeUnknownWithTheRotateHintAndTheOldTokenStays(t *testing.T) {
	for name, answer := range map[string]step{
		"connection lost":    {drop: true},
		"no pat":             ok(`{"pat_id":"p","scopes":[]}`),
		"empty pat":          ok(`{"pat":""}`),
		"pat with a space":   ok(`{"pat":"hsr1:eu:pat.aa bb","pat_id":"p"}`),
		"pat not hsr":        ok(`{"pat":"sk-live-1","pat_id":"p"}`),
		"pat with a newline": ok(`{"pat":"hsr1:eu:pat.aa\nbb","pat_id":"p"}`),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(rec recorded, _ int) step {
				if rec.path == "/v1/agent/credentials/rotate" {
					return answer
				}
				return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
			})
			c := srv.client(t)
			_, err := c.Agent.RotateToken(context.Background())
			e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
			if e.Hint != rotateHint || len(srv.to(rotateRoute)) != 1 {
				t.Errorf("hint %q, %d requests", e.Hint, len(srv.to(rotateRoute)))
			}
			if _, err := c.Balance(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := nth(t, srv.to("GET /v1/account"), 0).header.Get("Authorization"); got != "Bearer "+testToken {
				t.Errorf("the token changed after a failed rotation: %q", got)
			}
		})
	}
}

// The server revoked the old token as soon as it answered, so a usable token in an unusable answer
// still becomes the client's token, and is kept on the error.
func TestATokenInAnUnusableRotationAnswerIsKeptOnTheErrorAndSent(t *testing.T) {
	for name, answer := range map[string]step{
		"no pat_id":         ok(`{"pat":"` + newTokenText + `","scopes":[]}`),
		"scopes not a list": ok(`{"pat":"` + newTokenText + `","pat_id":"p","scopes":"transcribe"}`),
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(rec recorded, _ int) step {
				if rec.path == "/v1/agent/credentials/rotate" {
					return answer
				}
				return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
			})
			c := srv.client(t)
			_, err := c.Agent.RotateToken(context.Background())
			e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
			if e.ReceivedToken() != newTokenText || e.Hint != rotateHint {
				t.Errorf("ReceivedToken %q, hint %q", e.ReceivedToken(), e.Hint)
			}
			for _, shown := range []string{e.Error(), fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), e.Message} {
				if strings.Contains(shown, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
					t.Errorf("the token is shown: %q", shown)
				}
			}
			if _, err := c.Balance(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := nth(t, srv.to("GET /v1/account"), 0).header.Get("Authorization"); got != "Bearer "+newTokenText {
				t.Errorf("Authorization after the rotation = %q", got)
			}
		})
	}
}

func TestAClaimWithATokenButNoUserOrScopesKeepsTheTokenOnTheError(t *testing.T) {
	for name, body := range map[string]string{ //nolint:gosec // made-up tokens
		"only the token":   `{"pat":"` + newTokenText + `"}`,
		"scopes mistyped":  `{"pat":"` + newTokenText + `","user_id":"u","scopes":7,"balance_seconds":0}`,
		"balance mistyped": `{"pat":"` + newTokenText + `","user_id":"u","scopes":[],"balance_seconds":"0"}`,
		"balance null":     `{"pat":"` + newTokenText + `","user_id":"u","scopes":[],"balance_seconds":null}`,
		"bad token format": `{"pat":"not-a-token","user_id":"u","scopes":[],"balance_seconds":0}`,
	} {
		srv := newRouteServer(t, func(recorded, int) step { return ok(body) })
		_, err := AgentClaim(context.Background(), "sg_1", "cs", tokenless(srv)...)
		e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
		if e.Hint != claimHint || len(srv.to(claimRoute)) != 1 || strings.Contains(e.Error(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
			t.Errorf("%s: hint %q, %d requests, text %q", name, e.Hint, len(srv.to(claimRoute)), e.Error())
		}
		if name != "bad token format" && e.ReceivedToken() != newTokenText {
			t.Errorf("%s: ReceivedToken %q", name, e.ReceivedToken())
		}
	}
}

func TestEveryFriendlyReadRefusesAnEmptyObjectAndAWrongTypedFieldAsBadResponse(t *testing.T) {
	calls := map[string]func(*Client) error{
		"packs": func(c *Client) error { _, err := c.Billing.Packs(context.Background()); return err },
		"cards": func(c *Client) error { _, err := c.Billing.Cards(context.Background()); return err },
		"setup": func(c *Client) error { _, err := c.Billing.AddCardURL(context.Background()); return err },
		"transactions": func(c *Client) error {
			_, err := drain(c.Billing.Transactions(context.Background()))
			return err
		},
		"ledger":  func(c *Client) error { _, err := drain(c.Billing.Ledger(context.Background())); return err },
		"holds":   func(c *Client) error { _, err := drain(c.Billing.Holds(context.Background())); return err },
		"balance": func(c *Client) error { _, err := c.Balance(context.Background()); return err },
	}
	bodies := map[string][]string{
		"packs":        {`{}`, `{"packs":[{"id":"45min","seconds":"2700","amount":199,"currency":"usd","referral_bonus_seconds":0}]}`, `{"packs":[null]}`, `{"packs":[{}]}`, `[]`, `null`},
		"cards":        {`{}`, `{"default_id":null,"methods":[{"id":5}]}`, `{"default_id":null,"methods":[{}]}`, `{"default_id":3,"methods":[]}`, `[]`},
		"setup":        {`{}`, `{"checkout_url":7}`, `{"checkout_url":null}`, `[]`},
		"transactions": {`{}`, `{"transactions":[{"id":"x","created_at":1}]}`, `{"transactions":[{"id":1}]}`, `{"transactions":[null]}`},
		"ledger":       {`{}`, `{"entries":[{"id":1,"created_at":"x"}]}`, `{"entries":[{"id":1}]}`, `{"entries":[1]}`},
		"holds":        {`{}`, `{"holds":[{"id":1,"created_at":1}]}`, `{"holds":[{"id":"a"}]}`, `{"holds":"none"}`},
		"balance":      {`{}`, `{"balance_seconds":"x","held_seconds":0}`, `[]`},
	}
	for name, call := range calls {
		for _, body := range bodies[name] {
			api := newRouteServer(t, func(recorded, int) step { return ok(body) })
			err := call(api.client(t))
			if e := wantCode(t, err, KindAPI, "bad_response"); e.Status != 200 {
				t.Errorf("%s %s: status %d", name, body, e.Status)
			}
		}
	}
	for _, body := range []string{`{}`, `{"signup_id":1,"claim_secret":"s","checkout_url":"u","amount":1,"currency":"usd","expires_at":1}`} {
		srv := newRouteServer(t, func(recorded, int) step { return ok(body) })
		_, err := AgentSignup(context.Background(), request(), tokenless(srv)...)
		_ = wantCode(t, err, KindAPI, "bad_response")
	}
}

func TestAnUnknownCurrencyNeverBreaksTheFormatting(t *testing.T) {
	for _, code := range []string{"XYZ", "", "zz", "12", "usd "} {
		if got := newMoney(1234, code); got.Amount != 1234 || got.Formatted == "" || !strings.Contains(got.Formatted, "12.34") {
			t.Errorf("%q: %+v", code, got)
		}
	}
}

func TestRotateIsNeverRetriedOnAServerError(t *testing.T) {
	srv := newRouteServer(t, func(recorded, int) step { return step{status: 503, body: "x"} })
	_, err := srv.client(t).Agent.RotateToken(context.Background())
	if e := asError(t, err); e.Kind != KindAPI || e.Status != http.StatusServiceUnavailable || len(srv.to(rotateRoute)) != 1 {
		t.Errorf("error %+v, %d requests", e, len(srv.to(rotateRoute)))
	}
}

func TestTheAgentCallsOfAZeroClientFailWithAConfigurationError(t *testing.T) {
	_, err := (&Client{}).Agent.RotateToken(context.Background())
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
}

func TestConcurrentRequestsAndARotationShareOneTokenWithoutARace(t *testing.T) {
	srv := newRouteServer(t, func(rec recorded, _ int) step {
		if rec.path == "/v1/agent/credentials/rotate" {
			return rotateAnswer("null")
		}
		return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
	})
	c := srv.client(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _, _ = c.Balance(context.Background()) })
	}
	if _, err := c.Agent.RotateToken(context.Background()); err != nil {
		t.Error(err)
	}
	wg.Wait()
}

// ReceivedToken is a method, and the token sits behind a pointer in an unexported field: no fmt verb
// shows the text of it, on the error or on a copy of it, and neither does json or gob.
func TestAReceivedTokenIsShownByNoFormatVerbAndNoEncoder(t *testing.T) {
	answers := map[string]func(t *testing.T) *Error{
		"claim": func(t *testing.T) *Error {
			srv := newRouteServer(t, func(recorded, int) step { return ok(`{"pat":"` + newTokenText + `"}`) })
			_, err := AgentClaim(context.Background(), "sg_1", "cs", tokenless(srv)...)
			return wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
		},
		"rotate": func(t *testing.T) *Error {
			srv := newRouteServer(t, func(recorded, int) step { return ok(`{"pat":"` + newTokenText + `","scopes":[]}`) })
			_, err := srv.client(t).Agent.RotateToken(context.Background())
			return wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
		},
	}
	for name, raise := range answers {
		t.Run(name, func(t *testing.T) {
			e := raise(t)
			if e.ReceivedToken() != newTokenText {
				t.Fatalf("ReceivedToken() = %q", e.ReceivedToken())
			}
			var shown []string
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%T", "%p", "%10v", "%-20s"} {
				shown = append(shown, verb+" of the pointer: "+fmt.Sprintf(verb, e), verb+" of the value: "+fmt.Sprintf(verb, *e))
			}
			shown = append(shown, fmt.Sprint(e), fmt.Sprint(*e), fmt.Sprintln(e, *e), fmt.Sprint([]*Error{e}), fmt.Sprintf("%+v", map[string]Error{"k": *e}))
			for _, value := range []any{e, *e} {
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				shown = append(shown, string(encoded))
				var buffer bytes.Buffer
				_ = gob.NewEncoder(&buffer).Encode(value) // an encoding error is fine: nothing may leak either way
				shown = append(shown, buffer.String())
			}
			for _, text := range shown {
				for _, secret := range []string{newTokenText, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", hex.EncodeToString([]byte(newTokenText)), strings.ToUpper(hex.EncodeToString([]byte(newTokenText)))} {
					if strings.Contains(text, secret) {
						t.Errorf("the token is shown by %q", text)
					}
				}
			}
		})
	}
}

func TestARotationWhoseExpiryCannotBeReadKeepsTheOldTokenAndTheNewOneOnTheError(t *testing.T) {
	for name, expires := range map[string]string{
		"a string":            `"soon"`,
		"a fraction":          `1.5`,
		"a boolean":           `true`,
		"an object":           `{}`,
		"beyond the bound":    `8640000000001`,
		"before the bound":    `-8640000000001`,
		"far beyond a date":   `1000000000000000`,
		"too large for int64": `99999999999999999999`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(rec recorded, _ int) step {
				if rec.path == "/v1/agent/credentials/rotate" {
					return rotateAnswer(expires)
				}
				return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
			})
			c := srv.client(t)
			result, err := c.Agent.RotateToken(context.Background())
			e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
			if result != nil || e.ReceivedToken() != newTokenText || e.Hint != rotateHint {
				t.Errorf("result %+v, ReceivedToken %q, hint %q", result, e.ReceivedToken(), e.Hint)
			}
			if _, err := c.Balance(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := nth(t, srv.to("GET /v1/account"), 0).header.Get("Authorization"); got != "Bearer "+testToken {
				t.Errorf("the old token was swapped out: %q", got)
			}
		})
	}
}

// The answer is read the same whatever the 2xx status: a 201 or 203 with an expiry that cannot be
// read leaves the old token in place, exactly as a 200 does (TS parity).
func TestARotationAnswerIsJudgedOnItsBodyForEvery2xxStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNonAuthoritativeInfo} {
		for name, expires := range map[string]string{"a string": `"soon"`, "a fraction": `1.5`, "beyond the bound": `8640000000001`} {
			t.Run(fmt.Sprintf("%d %s", status, name), func(t *testing.T) {
				srv := newRouteServer(t, func(rec recorded, _ int) step {
					if rec.path == "/v1/agent/credentials/rotate" {
						answer := rotateAnswer(expires)
						answer.status = status
						return answer
					}
					return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
				})
				c := srv.client(t)
				result, err := c.Agent.RotateToken(context.Background())
				e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
				if result != nil || e.ReceivedToken() != newTokenText || e.Hint != rotateHint {
					t.Errorf("result %+v, ReceivedToken %q, hint %q", result, e.ReceivedToken(), e.Hint)
				}
				if _, err := c.Balance(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := nth(t, srv.to("GET /v1/account"), 0).header.Get("Authorization"); got != "Bearer "+testToken {
					t.Errorf("status %d: the old token was swapped out: %q", status, got)
				}
			})
		}
	}
}

func TestAUsableRotationOrClaimAnswerIsAcceptedWhatever2xxStatusCarriesIt(t *testing.T) {
	for _, status := range []int{http.StatusCreated, http.StatusAccepted, http.StatusNonAuthoritativeInfo} {
		rotated := newRouteServer(t, func(rec recorded, _ int) step {
			if rec.path == "/v1/agent/credentials/rotate" {
				answer := rotateAnswer("1790000000")
				answer.status = status
				return answer
			}
			return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
		})
		c := rotated.client(t)
		if got, err := c.Agent.RotateToken(context.Background()); err != nil || got.Token != newTokenText {
			t.Fatalf("rotate %d: %+v, %v", status, got, err)
		}
		if _, err := c.Balance(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := nth(t, rotated.to("GET /v1/account"), 0).header.Get("Authorization"); got != "Bearer "+newTokenText {
			t.Errorf("rotate %d: the new token was not adopted: %q", status, got)
		}
		claimed := newRouteServer(t, func(recorded, int) step {
			answer := claimAnswer()
			answer.status = status
			return answer
		})
		if got, err := AgentClaim(context.Background(), "sg_1", "cs", tokenless(claimed)...); err != nil || got.Token != newTokenText {
			t.Errorf("claim %d: %+v, %v", status, got, err)
		}
	}
}

func TestARotationExpiryAtTheBoundOrAbsentIsAccepted(t *testing.T) {
	for name, tc := range map[string]struct {
		expires string
		want    *time.Time
	}{
		"at the bound": {`8640000000000`, ptr(time.Unix(8_640_000_000_000, 0).UTC())},
		"zero":         {`0`, ptr(time.Unix(0, 0).UTC())},
		"null":         {`null`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return rotateAnswer(tc.expires) })
			got, err := srv.client(t).Agent.RotateToken(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (got.ExpiresAt == nil) != (tc.want == nil) || got.ExpiresAt != nil && !got.ExpiresAt.Equal(*tc.want) {
				t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, tc.want)
			}
		})
	}
}

func ptr[T any](value T) *T { return &value }

func TestAnAnswerWhoseTokenIsOnlyWhitespaceLeavesNoReceivedToken(t *testing.T) {
	for _, pat := range []string{``, `  `, ` \n\t `} {
		body := `{"pat":"` + pat + `","pat_id":"p","scopes":[],"user_id":"u","balance_seconds":0}`
		srv := newRouteServer(t, func(recorded, int) step { return ok(body) })
		_, err := AgentClaim(context.Background(), "sg_1", "cs", tokenless(srv)...)
		if e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown"); e.ReceivedToken() != "" {
			t.Errorf("claim, pat %q: ReceivedToken %q", pat, e.ReceivedToken())
		}
		_, err = srv.client(t).Agent.RotateToken(context.Background())
		if e := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown"); e.ReceivedToken() != "" {
			t.Errorf("rotate, pat %q: ReceivedToken %q", pat, e.ReceivedToken())
		}
	}
}

// R79: the three results that hand the caller a secret keep it as a readable field, show [redacted] in
// its place in every text form, and carry it through json.Marshal (documented).
func TestAResultThatHoldsASecretPrintsRedactedInPlaceOfItAndEncodesIt(t *testing.T) {
	const secretText = "s3cr3t-value-0123456789"
	expires := time.Unix(1_760_001_800, 0).UTC()
	signup := AgentSignupResult{SignupID: "sg_1", ClaimSecret: secretText, CheckoutURL: "https://pay.example/c/1", ExpiresAt: expires}
	account := AgentAccount{Token: secretText, UserID: "u_1", Scopes: []string{"read"}}
	rotated := RotatedToken{Token: secretText, TokenID: "tk_1", Scopes: []string{"read"}, ExpiresAt: &expires}
	cases := []struct {
		name, typ string
		field     string
		value     any
		pointer   any
	}{
		{"signup result", "AgentSignupResult", signup.ClaimSecret, signup, &signup},
		{"account", "AgentAccount", account.Token, account, &account},
		{"rotated token", "RotatedToken", rotated.Token, rotated, &rotated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.field != secretText {
				t.Fatal("the field is not readable")
			}
			for form, v := range map[string]any{"value": tc.value, "pointer": tc.pointer} {
				for _, wrapped := range []any{v, []any{v}, map[string]any{"k": v}, struct{ V any }{v}} {
					for _, verb := range leakVerbs {
						if got := fmt.Sprintf(verb, wrapped); strings.Contains(got, secretText) {
							t.Errorf("%s of the %s printed the secret: %s", verb, form, got)
						}
					}
				}
				for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
					if got := fmt.Sprintf(verb, v); !strings.Contains(got, redactedField) || !strings.Contains(got, tc.typ) {
						t.Errorf("%s of the %s = %q, want the type and %s", verb, form, got, redactedField)
					}
				}
				var logged bytes.Buffer
				slog.New(slog.NewTextHandler(&logged, nil)).Info("m", "result", v)
				if strings.Contains(logged.String(), secretText) {
					t.Errorf("the slog text handler logged the secret for the %s: %s", form, logged.String())
				}
				if encoded, err := json.Marshal(v); err != nil || !strings.Contains(string(encoded), secretText) {
					t.Errorf("json.Marshal of the %s = %s, %v: the value must ride along", form, encoded, err)
				}
			}
		})
	}
}

func TestCanceledAgentCallsSendNothingAndKeepTheOldToken(t *testing.T) {
	calls := map[string]func(context.Context, *Client, *routeServer) error{
		"signup": func(ctx context.Context, _ *Client, srv *routeServer) error {
			_, err := AgentSignup(ctx, request(), tokenless(srv)...)
			return err
		},
		"claim": func(ctx context.Context, _ *Client, srv *routeServer) error {
			_, err := AgentClaim(ctx, "sg_1", "cs_secret", tokenless(srv)...)
			return err
		},
		"rotate": func(ctx context.Context, c *Client, _ *routeServer) error {
			_, err := c.Agent.RotateToken(ctx)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(rec recorded, _ int) step {
				switch rec.method + " " + rec.path {
				case signupRoute:
					return signupAnswer()
				case claimRoute:
					return claimAnswer()
				case rotateRoute:
					return rotateAnswer("null")
				}
				return step{status: 404, body: jsonError("not_found", 4040)}
			})
			c := srv.client(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			e := wantCode(t, call(ctx, c, srv), KindAbort, "aborted")
			if e.Status != 0 || len(srv.all()) != 0 || c.core.cfg.tokens.get() != testToken || !errors.Is(e, context.Canceled) {
				t.Errorf("status %d, requests %v, token changed %v, cause %v", e.Status, srv.all(), c.core.cfg.tokens.get() != testToken, e.Unwrap())
			}
		})
	}
}

func TestAgentClaimIDsAreRejectedBeforeAnyRequest(t *testing.T) {
	for _, id := range []string{"", ".", ".."} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			srv := newScriptedServer(t, okBody)
			_, err := AgentClaim(context.Background(), id, "claim-secret", WithBaseURL(srv.URL), WithClock(newFakeClock()))
			assertCoreError(t, err, KindPreflight, "invalid_option", 0)
			if srv.count() != 0 {
				t.Errorf("invalid signup id sent %d requests", srv.count())
			}
		})
	}
}

func TestAgentResultsKeepTheirUnknownFieldsInRawButNotInTheirTextForms(t *testing.T) {
	future := `"x_future":{"deeper":[1,null,"x"]}`
	signup := `{"signup_id":"hsr1:eu:abc123","claim_secret":"cs_secret","checkout_url":"https://pay.example/c/1","amount":199,"currency":"usd","expires_at":1760001800,` + future + `}`
	claim := `{"pat":"` + newTokenText + `","user_id":"u_1","scopes":["transcribe"],"balance_seconds":2700,` + future + `}`
	rotate := `{"pat":"` + newTokenText + `","pat_id":"pat_2","scopes":["transcribe"],"expires_at":null,` + future + `}`
	srv := newRouteServer(t, func(rec recorded, _ int) step {
		switch rec.method + " " + rec.path {
		case signupRoute:
			return ok(signup)
		case claimRoute:
			return ok(claim)
		case rotateRoute:
			return ok(rotate)
		}
		return step{status: 404, body: jsonError("not_found", 4040)}
	})
	signedUp, err := AgentSignup(context.Background(), request(), tokenless(srv)...)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := AgentClaim(context.Background(), "sg_1", "cs_secret", tokenless(srv)...)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := newTestClient(t, srv.URL, newFakeClock(), WithToken(testToken)).Agent.RotateToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		result any
		want   string
		secret string
	}{
		{"signup", *signedUp, signup, "cs_secret"},
		{"claim", *claimed, claim, newTokenText},
		{"rotate", *rotated, rotate, newTokenText},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nestedRaw(t, tc.result); got != tc.want {
				t.Errorf("Raw = %s, want %s", got, tc.want)
			}
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%q"} {
				text := fmt.Sprintf(verb, tc.result)
				for _, leak := range []string{tc.secret, hex.EncodeToString([]byte(tc.secret)), fmt.Sprintf("%d", []byte(tc.secret)), "x_future", fmt.Sprintf("%d", []byte(`"x_future"`))} {
					if strings.Contains(text, leak) {
						t.Errorf("%s shows %q: %s", verb, leak, text)
					}
				}
			}
		})
	}
}

type echoedBodyError struct{ body string }

func (e *echoedBodyError) Error() string { return "transport echoed " + e.body }

func TestAClaimSecretEchoedByTheTransportOrTheAPIIsRedactedEverywhere(t *testing.T) {
	const claimSecret = "cs_very_secret_value" //nolint:gosec // a made-up secret
	t.Run("transport error", func(t *testing.T) {
		transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			sent, _ := io.ReadAll(req.Body)
			return nil, &echoedBodyError{body: string(sent)}
		})
		_, err := AgentClaim(context.Background(), "sg_1", claimSecret, WithBaseURL("http://127.0.0.1:1"), WithHTTPClient(&http.Client{Transport: transport}))
		failure := wantCode(t, err, KindOutcomeUnknown, "outcome_unknown")
		cause := failure.Unwrap()
		if cause == nil {
			t.Fatal("no cause")
		}
		for what, text := range map[string]string{
			"Message": failure.Message, "Hint": failure.Hint, "Error()": failure.Error(), "cause": cause.Error(),
			"%v": fmt.Sprintf("%v", failure), "%+v": fmt.Sprintf("%+v", failure), "%#v": fmt.Sprintf("%#v", failure),
		} {
			if strings.Contains(text, claimSecret) {
				t.Errorf("%s shows the claim secret: %s", what, text)
			}
		}
		if want := `Post "http://127.0.0.1:1/v1/agent/accounts/sg_1/claim": transport echoed {"claim_secret":"[redacted]"}`; cause.Error() != want {
			t.Errorf("cause = %q, want %q", cause.Error(), want)
		}
		var echoed *echoedBodyError
		if errors.As(err, &echoed) {
			t.Errorf("errors.As handed out the transport's own error: %v", echoed)
		}
	})
	t.Run("api error body", func(t *testing.T) {
		srv := newRouteServer(t, func(recorded, int) step {
			return step{status: 400, body: `{"error":"bad_request","code":4000,"detail":"you sent ` + claimSecret + `"}`}
		})
		_, err := AgentClaim(context.Background(), "sg_1", claimSecret, tokenless(srv)...)
		failure := wantCode(t, err, KindValidation, "bad_request")
		if strings.Contains(fmt.Sprint(failure.Details), claimSecret) || failure.Details["detail"] != "you sent [redacted]" {
			t.Errorf("details = %v", failure.Details)
		}
	})
	t.Run("a blank claim secret redacts nothing", func(t *testing.T) {
		srv := newRouteServer(t, func(recorded, int) step { return step{status: 400, body: jsonError("bad_request", 4000)} })
		_, err := AgentClaim(context.Background(), "sg_1", " ", tokenless(srv)...)
		failure := wantCode(t, err, KindValidation, "bad_request")
		_, err = AgentClaim(context.Background(), "sg_1", claimSecret, tokenless(srv)...)
		want := wantCode(t, err, KindValidation, "bad_request")
		if failure.Message != want.Message || failure.Hint != want.Hint || strings.Contains(failure.Message, redactedField) {
			t.Errorf("blank secret: message %q hint %q, want message %q hint %q", failure.Message, failure.Hint, want.Message, want.Hint)
		}
	})
}
