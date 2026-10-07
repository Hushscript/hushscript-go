package hushscript

import (
	"context"
	"fmt"
	"testing"
)

// twoXXCall is one SDK call with the path it hits, a body the SDK can use and a body it cannot.
type twoXXCall struct {
	name string
	path string
	good string
	bad  string
	call func(c *Client) error
}

func twoXXCalls() []twoXXCall {
	ctx := context.Background()
	return []twoXXCall{
		{
			"Account", "/v1/account", fmt.Sprintf(accountJSON(""), 5, 0, "null"), `{"balance_seconds":"x"}`,
			func(c *Client) error { _, err := c.Account(ctx); return err },
		},
		{
			"Balance", "/v1/account", fmt.Sprintf(accountJSON(""), 5, 0, "null"), `{}`,
			func(c *Client) error { _, err := c.Balance(ctx); return err },
		},
		{
			"Jobs.Get", "/v1/jobs/j1", jobJSON("j1", "done", ""), `{"id":"j1"}`,
			func(c *Client) error { _, err := c.Jobs.Get(ctx, "j1"); return err },
		},
		{
			"Jobs.List", "/v1/jobs", `{"jobs":[` + jobJSON("j1", "done", "") + `],"next_cursor":null}`, `{"jobs":[{"id":"j1"}]}`,
			func(c *Client) error { _, err := drain(c.Jobs.List(ctx, nil)); return err },
		},
		{
			"Transcripts.List", "/v1/transcripts", rowsPage(1, 1, ""), `{"transcripts":[{"id":5}]}`,
			func(c *Client) error { _, err := drain(c.Transcripts.List(ctx, nil)); return err },
		},
		{
			"Transcripts.Page", "/v1/transcripts", rowsPage(1, 1, ""), `{}`,
			func(c *Client) error { _, err := c.Transcripts.Page(ctx, nil); return err },
		},
		{
			"Transcripts.Get", "/v1/transcripts/tr_9",
			`{"id":"tr_9","title":"T","duration_seconds":3,"tags":[],"created_at":1,"updated_at":2,"body":{"version":1,"original":{"utterances":[]},"translations":{}}}`, `{"id":"tr_9"}`,
			func(c *Client) error { _, err := c.Transcripts.Get(ctx, "tr_9"); return err },
		},
		{
			"Transcripts.Deleted", "/v1/transcripts/deleted",
			`{"deleted":[{"id":"d1","title":"A","duration_seconds":30,"created_at":1760000000,"deleted_at":1760000500,"cause":"manual","receipt":null}],"next_cursor":null}`, `{"deleted":[{"id":"d1"}]}`,
			func(c *Client) error { _, err := drain(c.Transcripts.Deleted(ctx)); return err },
		},
		{
			"Billing.Packs", "/v1/billing/packs", `{"packs":[{"id":"45min","seconds":2700,"amount":199,"currency":"usd","referral_bonus_seconds":0}]}`, `{"packs":[{"id":"45min"}]}`,
			func(c *Client) error { _, err := c.Billing.Packs(ctx); return err },
		},
		{
			"Billing.Cards", "/v1/billing/cards", defaultCard, `{"default_id":null}`,
			func(c *Client) error { _, err := c.Billing.Cards(ctx); return err },
		},
		{
			"Billing.AddCardURL", "/v1/billing/setup-session", `{"checkout_url":"https://pay.example/setup","expires_at":1760000000,"session_id":"cs_1"}`, `{}`,
			func(c *Client) error { _, err := c.Billing.AddCardURL(ctx); return err },
		},
		{
			"Billing.Buy", "/v1/billing/purchase", `{"status":"ok","balance_seconds":600}`, `{"status":"ok"}`,
			func(c *Client) error { _, err := c.Billing.Buy(ctx, "45min", nil); return err },
		},
		{
			"Billing.Transactions", "/v1/billing/transactions",
			`{"account_reference":"acc","transactions":[{"id":7,"seconds":18000,"amount_cents":599,"amount_refunded_cents":0,"currency":"usd","status":"succeeded","created_at":1760000000,"payment_method_type":"card","payment_method_brand":"visa","payment_method_last4":"4242","receipt_number":"HS-1","initiated_via":null}],"next_cursor":null}`,
			`{"transactions":[{"id":7}]}`,
			func(c *Client) error { _, err := drain(c.Billing.Transactions(ctx)); return err },
		},
		{
			"Billing.Ledger", "/v1/billing/ledger", `{"entries":[{"id":3,"delta_seconds":-60,"kind":"settle","job_id":"j1","note":null,"created_at":1760000001}],"next_cursor":null}`, `{"entries":[{"id":3}]}`,
			func(c *Client) error { _, err := drain(c.Billing.Ledger(ctx)); return err },
		},
		{
			"Billing.Holds", "/v1/billing/holds", `{"holds":[{"id":"j1","kind":"transcription","seconds":120,"created_at":1760000002}],"next_cursor":null}`, `{"holds":[{"id":"j1"}]}`,
			func(c *Client) error { _, err := drain(c.Billing.Holds(ctx)); return err },
		},
	}
}

// twoXXServer answers the call's route (and the cards list a purchase reads first) with a body and a
// status; every other route is a 404.
func twoXXServer(t *testing.T, tc twoXXCall, status int, body string) *routeServer {
	t.Helper()
	return newRouteServer(t, func(rec recorded, _ int) step {
		switch {
		case rec.path == tc.path:
			return step{status: status, body: body}
		case rec.path == "/v1/billing/cards":
			return step{status: status, body: defaultCard}
		}
		return step{status: 404, body: jsonError("not_found", 4040)}
	})
}

// Success is any 2xx with a usable body (spec section 7.4, R58), on every operation.
func TestAnyTwoXXWithAUsableBodyIsASuccessOnEveryOperation(t *testing.T) {
	for _, status := range []int{200, 201, 202, 203} {
		for _, tc := range twoXXCalls() {
			t.Run(fmt.Sprintf("%s %d", tc.name, status), func(t *testing.T) {
				api := twoXXServer(t, tc, status, tc.good)
				if err := tc.call(api.client(t)); err != nil {
					t.Fatalf("a %d with a usable body: %v", status, err)
				}
			})
		}
	}
}

// A 2xx whose body the SDK cannot use is bad_response with the status the API really answered with.
func TestATwoXXWithAnUnusableBodyIsBadResponseWithThatStatusOnEveryOperation(t *testing.T) {
	for _, status := range []int{200, 201, 202, 203} {
		for _, tc := range twoXXCalls() {
			t.Run(fmt.Sprintf("%s %d", tc.name, status), func(t *testing.T) {
				api := twoXXServer(t, tc, status, tc.bad)
				err := tc.call(api.client(t))
				e := wantCode(t, err, KindAPI, "bad_response")
				if e.Status != status {
					t.Errorf("status %d, want %d", e.Status, status)
				}
			})
		}
	}
}

func TestASignupAnsweredWith201IsReadLikeA200(t *testing.T) {
	for _, status := range []int{200, 201, 202, 203} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return step{status: status, body: signupAnswer().body} })
			got, err := AgentSignup(context.Background(), request(), tokenless(srv)...)
			if err != nil || got.SignupID == "" || got.CheckoutURL == "" {
				t.Fatalf("signup answered %d: %+v, %v", status, got, err)
			}
			bad := newRouteServer(t, func(recorded, int) step { return step{status: status, body: `{"signup_id":"x"}`} })
			_, err = AgentSignup(context.Background(), request(), tokenless(bad)...)
			if e := wantCode(t, err, KindAPI, "bad_response"); e.Status != status {
				t.Errorf("status %d, want %d", e.Status, status)
			}
		})
	}
}
