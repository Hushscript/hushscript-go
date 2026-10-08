package hushscript

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

// AgentInfo describes the agent an account is opened for.
type AgentInfo struct {
	Name     string
	Platform string
	// ContactURL is optional.
	ContactURL string
}

// AgentSignupRequest is the request of AgentSignup.
type AgentSignupRequest struct {
	// Email is for receipts and notices only; it never signs in.
	Email string
	// Pack is 45min or 300min: a new agent account can buy only those two until earlier purchases settle.
	Pack string
	// AcceptTerms is the policy version being accepted. It is never defaulted, and an empty one is
	// refused before any request. A stale one fails with policy_version_stale, and the error's Hint
	// names the current version.
	AcceptTerms string
	Agent       AgentInfo
	// DataRegion is an optional hint, honored only when it agrees with the region derived from the network.
	DataRegion string
	// DryRun validates the request without opening a checkout; the result cannot be claimed.
	DryRun bool
}

// AgentSignupResult is what AgentSignup returns.
type AgentSignupResult struct {
	// SignupID is passed back to AgentClaim verbatim.
	SignupID string
	// ClaimSecret is shown once; AgentClaim needs it.
	ClaimSecret string
	// CheckoutURL is the hosted checkout page where the account is paid for; it expires at ExpiresAt.
	CheckoutURL string
	Price       Money
	ExpiresAt   time.Time
	// Raw is the answer as the API sent it, unknown fields included. It holds the claim secret, so no
	// text form of the result shows it.
	Raw json.RawMessage
}

// AgentAccount is what AgentClaim returns.
type AgentAccount struct {
	// Token is the account's token, shown once: write it down before anything else can fail.
	Token  string
	UserID string
	Scopes []string
	// Balance is the pack just granted. A new account holds nothing and has no expiry in the answer.
	Balance Balance
	// Raw is the answer as the API sent it, unknown fields included. It holds the token, so no text form
	// of the result shows it.
	Raw json.RawMessage
}

// RotatedToken is what RotateToken returns.
type RotatedToken struct {
	// Token is the successor token, shown once; the Client already sends it.
	Token   string
	TokenID string
	Scopes  []string
	// ExpiresAt is nil when the token never expires.
	ExpiresAt *time.Time
	// Raw is the answer as the API sent it, unknown fields included. It holds the successor token, so no
	// text form of the result shows it.
	Raw json.RawMessage
}

// redactedField stands in for a secret field in every text form of a result.
const redactedField = "[redacted]"

// String shows the result with ClaimSecret replaced by [redacted]; the field itself stays readable.
func (r AgentSignupResult) String() string {
	type plain AgentSignupResult // no methods, so printing it does not come back here
	r.ClaimSecret, r.Raw = redactedField, nil
	return fmt.Sprintf("hushscript.AgentSignupResult%+v", plain(r))
}

// GoString is String, so %#v never shows the claim secret either.
func (r AgentSignupResult) GoString() string { return r.String() }

// Format covers every fmt verb, including %d and %x, which would otherwise print the struct fields.
func (r AgentSignupResult) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

// String shows the account with Token replaced by [redacted]; the field itself stays readable.
func (a AgentAccount) String() string {
	type plain AgentAccount
	a.Token, a.Raw = redactedField, nil
	return fmt.Sprintf("hushscript.AgentAccount%+v", plain(a))
}

// GoString is String, so %#v never shows the token either.
func (a AgentAccount) GoString() string { return a.String() }

// Format covers every fmt verb, including %d and %x, which would otherwise print the struct fields.
func (a AgentAccount) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, a.String()) }

// String shows the rotation with Token replaced by [redacted]; the field itself stays readable.
func (r RotatedToken) String() string {
	type plain RotatedToken
	r.Token, r.Raw = redactedField, nil
	return fmt.Sprintf("hushscript.RotatedToken%+v", plain(r))
}

// GoString is String, so %#v never shows the successor token either.
func (r RotatedToken) GoString() string { return r.String() }

// Format covers every fmt verb, including %d and %x, which would otherwise print the struct fields.
func (r RotatedToken) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, r.String()) }

// AgentService is Client.Agent: the call an agent account makes with its own token.
type AgentService struct {
	core *core
}

const (
	signupOp = "POST /v1/agent/accounts"
	claimOp  = "POST /v1/agent/accounts/{signup_id}/claim"
	rotateOp = "POST /v1/agent/credentials/rotate"
)

// lostCredential is the error for a claim or rotation whose answer could not be used: the credential
// was minted and may be lost, exactly as if the answer never arrived (spec 7.4). token is whatever the
// answer carried in its pat field, kept on the error so it is not lost.
func (c *core) lostCredential(route string, status int, hint, token string) *Error {
	failure := newSDKError(KindOutcomeUnknown, "outcome_unknown",
		"The answer to "+route+" could not be used, so the token may be lost.", c.badResponse(route, status), c.cfg.redactor(), hintInputs{})
	failure.Hint = hint
	if strings.TrimSpace(token) != "" {
		failure.receivedToken = newSecret(token)
	}
	return failure
}

// credentialAnswer is a claim or rotation answer, read strictly: every field the SDK maps must be
// there with the right type, and a token that passes the token check.
type credentialAnswer struct {
	Pat            *string         `json:"pat"`
	UserID         *string         `json:"user_id"`
	PatID          *string         `json:"pat_id"`
	Scopes         *[]string       `json:"scopes"`
	BalanceSeconds *int            `json:"balance_seconds"`
	ExpiresAt      json.RawMessage `json:"expires_at"`
	raw            json.RawMessage
}

// maxUnixSeconds is the newest instant any SDK accepts as a timestamp, the span of a JavaScript Date
// (the TypeScript SDK's bound), so a value one SDK takes for a date is a date to all of them.
const maxUnixSeconds = 8_640_000_000_000

// expiry reads expires_at: ok is false for a value that is not an integer number of seconds within
// the bound; at is nil when the answer carried none (absent or null).
func (a credentialAnswer) expiry() (at *time.Time, ok bool) {
	text := strings.TrimSpace(string(a.ExpiresAt))
	if text == "" || text == "null" {
		return nil, true
	}
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil || seconds > maxUnixSeconds || seconds < -maxUnixSeconds {
		return nil, false
	}
	found := unixTime(seconds)
	return &found, true
}

// readCredential reads the 2xx answer of a claim or rotation. It returns the token string whenever
// the answer carried one, even when the rest was unusable, and ok only for a usable answer. The
// answer is decoded and checked whatever the 2xx status, so a 201 or 203 is judged on its body.
func (c *core) readCredential(resp *http.Response, required func(credentialAnswer) bool) (answer credentialAnswer, seen string, ok bool) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return answer, "", false
	}
	var loose map[string]any
	if json.Unmarshal(body, &loose) == nil {
		seen, _ = loose["pat"].(string)
	}
	answer.raw = bytes.Clone(body)
	if json.Unmarshal(body, &answer) != nil || answer.Pat == nil || !acceptableToken(*answer.Pat) || !required(answer) {
		return answer, seen, false
	}
	return answer, seen, true
}

// AgentSignup opens an agent account. It needs no token: a token from an option or HUSHSCRIPT_TOKEN is
// not sent. The agent then has the checkout paid, at CheckoutURL, and claims the account with AgentClaim.
// Options are the ones of New that apply (WithBaseURL, WithHTTPClient, WithTimeout, WithLogger and kin).
// The request is never retried; a lost answer is an OutcomeUnknown error.
func AgentSignup(ctx context.Context, req AgentSignupRequest, opts ...Option) (*AgentSignupResult, error) {
	cfg, err := resolveTokenless(opts)
	if err != nil {
		return nil, err
	}
	if req.AcceptTerms == "" {
		return nil, newSDKError(KindValidation, "accept_terms_required",
			"AcceptTerms is required: pass the policy version you accept. It is never filled in for you.", nil, cfg.redactor(), hintInputs{})
	}
	core, err := newCore(cfg)
	if err != nil {
		return nil, err
	}
	body := generated.CreateAgentAccountSignupJSONRequestBody{ContactEmail: req.Email, PackId: req.Pack, AcceptTermsVersion: req.AcceptTerms}
	body.Agent.Name, body.Agent.Platform = req.Agent.Name, req.Agent.Platform
	if req.Agent.ContactURL != "" {
		body.Agent.ContactUrl = &req.Agent.ContactURL
	}
	if req.DataRegion != "" {
		body.DataRegion = &req.DataRegion
	}
	if req.DryRun {
		body.DryRun = &req.DryRun
	}
	response, err := core.api.CreateAgentAccountSignupWithResponse(ctx, body)
	if err != nil {
		return nil, (&annotator{core: core}).annotate(ctx, core.readFailed(err, signupOp))
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := core.unusable(signupOp, response.HTTPResponse, parsed != nil, response.Body,
		"signup_id", "claim_secret", "checkout_url", "amount", "currency", "expires_at"); failure != nil {
		return nil, failure
	}
	signup := parsed
	return &AgentSignupResult{
		SignupID: signup.SignupId, ClaimSecret: signup.ClaimSecret, CheckoutURL: signup.CheckoutUrl,
		Price: newMoney(signup.Amount, signup.Currency), ExpiresAt: unixTime(int64(signup.ExpiresAt)), Raw: bytes.Clone(response.Body),
	}, nil
}

// AgentClaim claims a paid agent signup and returns the account's token, which the server shows once.
// Write it down before doing anything else. It needs no token and takes the same options as
// AgentSignup. The claim answers wrong_state (a KindConflict error) until the checkout is paid. It is
// never retried: a lost answer, or one without a token, is an OutcomeUnknown error whose Hint says what
// to check.
func AgentClaim(ctx context.Context, signupID, claimSecret string, opts ...Option) (*AgentAccount, error) {
	cfg, err := resolveTokenless(opts)
	if err != nil {
		return nil, err
	}
	cfg.secrets = []secret{newSecret(claimSecret)} // redacted from every error of this call, like the token
	if idErr := checkID(cfg.redactor(), "The signup id", signupID); idErr != nil {
		return nil, idErr
	}
	core, err := newCore(cfg)
	if err != nil {
		return nil, err
	}
	resp, err := core.api.ClaimAgentAccount(ctx, signupID, generated.ClaimAgentAccountJSONRequestBody{ClaimSecret: claimSecret})
	if err != nil {
		return nil, (&annotator{core: core}).annotate(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, seen, ok := core.readCredential(resp, func(a credentialAnswer) bool {
		return a.UserID != nil && a.Scopes != nil && a.BalanceSeconds != nil
	})
	if !ok {
		return nil, core.lostCredential(claimOp, resp.StatusCode, claimOutcomeHint, seen)
	}
	return &AgentAccount{
		Token: *answer.Pat, UserID: *answer.UserID, Scopes: *answer.Scopes,
		Balance: Balance{Seconds: *answer.BalanceSeconds, Minutes: *answer.BalanceSeconds / secondsPerMinute}, Raw: answer.raw,
	}, nil
}

// RotateToken mints a successor token and revokes the one in use. The Client sends the new token from
// then on, to retries already queued as well; store the returned one right away. It is never retried:
// a lost answer, or one without a usable token, is an OutcomeUnknown error whose Hint says what to
// check, and the Client keeps its old token then.
func (a AgentService) RotateToken(ctx context.Context) (*RotatedToken, error) {
	if a.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	core := a.core
	resp, err := core.api.RotateAgentCredentials(ctx)
	if err != nil {
		return nil, (&annotator{core: core}).annotate(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, seen, ok := core.readCredential(resp, func(a credentialAnswer) bool {
		_, usable := a.expiry()
		return a.PatID != nil && a.Scopes != nil && usable
	})
	// The old token is revoked as soon as the server answered: from here on the client sends the new
	// one. An expiry that cannot be read is the exception, as in the TypeScript SDK: the answer is not
	// trusted at all, so the old token stays in use and the new one is only kept on the error.
	if _, usable := answer.expiry(); usable && acceptableToken(seen) && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		core.cfg.tokens.swap(seen)
	}
	if !ok {
		return nil, core.lostCredential(rotateOp, resp.StatusCode, rotateOutcomeHint, seen)
	}
	out := &RotatedToken{Token: *answer.Pat, TokenID: *answer.PatID, Scopes: *answer.Scopes, Raw: answer.raw}
	out.ExpiresAt, _ = answer.expiry()
	return out, nil
}
