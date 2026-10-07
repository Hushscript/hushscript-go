package hushscript

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		if name == "drop" && (e.Kind != KindOutcomeUnknown || e.Hint != "" || e.Unwrap() == nil) {
			t.Errorf("drop: %+v", e)
		}
		if name == "503" && e.Kind != KindAPI {
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
	if got := srv.all()[0].path; got != "/v1/agent/accounts/hsr1:eu:abc%2F123/claim" {
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
			if got := srv.to("GET /v1/account")[0].header.Get("Authorization"); got != "Bearer "+testToken {
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
		"202":               {status: 202, body: `{"pat":"` + newTokenText + `","pat_id":"p","scopes":[]}`},
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
			if e.UnusableToken != newTokenText || e.Hint != rotateHint {
				t.Errorf("UnusableToken %q, hint %q", e.UnusableToken, e.Hint)
			}
			for _, shown := range []string{e.Error(), fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), e.Message} {
				if strings.Contains(shown, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
					t.Errorf("the token is shown: %q", shown)
				}
			}
			if _, err := c.Balance(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := srv.to("GET /v1/account")[0].header.Get("Authorization"); got != "Bearer "+newTokenText {
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
		if name != "bad token format" && e.UnusableToken != newTokenText {
			t.Errorf("%s: UnusableToken %q", name, e.UnusableToken)
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
