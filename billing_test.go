package hushscript

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPurchaseKeepsItsKeyUnlessEveryAttemptWasUnsent(t *testing.T) {
	setup := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	lost := errors.New("response lost")
	for _, token := range []string{testToken, "hsr1:agent:pat.0123456789abcdef.0123456789abcdef"} {
		for _, tc := range []struct {
			name    string
			first   error
			status  int
			cancel  bool
			wantKey bool
		}{
			{name: "all setup failures", first: setup},
			{name: "lost response then setup failure", first: lost, wantKey: true},
			{name: "503 then setup failure", status: 503, wantKey: true},
			{name: "cancelled before purchase", cancel: true},
		} {
			t.Run(token[:10]+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				transport := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet {
						if tc.cancel {
							cancel()
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(defaultCard))}, nil
					}
					calls++
					if calls == 1 {
						if tc.status != 0 {
							return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
						}
						return nil, tc.first
					}
					return nil, setup
				})}
				c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithToken(token), WithHTTPClient(transport))
				_, err := c.Buy(ctx, "45min", &BuyOptions{IdempotencyKey: "purchase-key"})
				e := asError(t, err)
				if (e.IdempotencyKey == "purchase-key") != tc.wantKey {
					t.Errorf("key = %q, want preserved=%v", e.IdempotencyKey, tc.wantKey)
				}
				if strings.Contains(e.Message, "before the request was sent") == tc.wantKey {
					t.Errorf("message = %q, want unsent=%v", e.Message, !tc.wantKey)
				}
				wantCalls := 2
				if tc.cancel {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Errorf("purchase attempts = %d, want %d", calls, wantCalls)
				}
			})
		}
	}
}

// routeServer answers every request through one handler and records them all, headers and bodies
// included. A step with drop closes the connection after the request was read.
type routeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
}

func newRouteServer(t *testing.T, handler func(rec recorded, nth int) step) *routeServer {
	t.Helper()
	s := &routeServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recorded{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(body)}
		s.mu.Lock()
		s.requests = append(s.requests, rec)
		nth := 0
		for _, earlier := range s.requests {
			if earlier.method == rec.method && earlier.path == rec.path {
				nth++
			}
		}
		s.mu.Unlock()
		current := handler(rec, nth)
		if current.delay > 0 {
			select {
			case <-time.After(current.delay):
			case <-r.Context().Done():
				return
			}
		}
		if current.drop {
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		for key, value := range current.headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(current.status)
		_, _ = io.WriteString(w, current.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *routeServer) all() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.requests...)
}

// to lists the requests that went to one route, like "POST /v1/billing/purchase".
func (s *routeServer) to(route string) []recorded {
	var out []recorded
	for _, rec := range s.all() {
		if rec.method+" "+rec.path == route {
			out = append(out, rec)
		}
	}
	return out
}

func (s *routeServer) client(t *testing.T) *Client {
	t.Helper()
	return newTestClient(t, s.URL, newFakeClock())
}

const (
	purchaseRoute = "POST /v1/billing/purchase"
	cardsRoute    = "GET /v1/billing/cards"
)

func ok(body string) step { return step{status: 200, body: body} }

func cardsBody(defaultID string, cards ...string) string {
	def := "null"
	if defaultID != "" {
		def = `"` + defaultID + `"`
	}
	return `{"default_id":` + def + `,"methods":[` + strings.Join(cards, ",") + `]}`
}

func card(id string, isDefault, eligible bool) string {
	b, _ := json.Marshal(map[string]any{
		"id": id, "type": "card", "brand": "visa", "last4": "4242", "exp_month": 4, "exp_year": 2030, "email": nil,
		"is_default": isDefault, "fast_checkout_eligible": eligible,
	})
	return string(b)
}

// buyServer answers the cards list with cards and every purchase with purchases (the last repeats).
func buyServer(t *testing.T, cards string, purchases ...step) *routeServer {
	t.Helper()
	return newRouteServer(t, func(rec recorded, nth int) step {
		switch rec.method + " " + rec.path {
		case cardsRoute:
			return ok(cards)
		case purchaseRoute:
			return purchases[min(nth-1, len(purchases)-1)]
		}
		return step{status: 404, body: jsonError("not_found", 4040)}
	})
}

var defaultCard = cardsBody("pm_1", card("pm_1", true, true))

func purchaseBody(t *testing.T, rec recorded) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(rec.body), &body); err != nil {
		t.Fatalf("purchase body %q: %v", rec.body, err)
	}
	return body
}

func TestBuyChargesTheDefaultCardAndReturnsThePurchaseFromThePurchaseAnswerAlone(t *testing.T) {
	api := buyServer(t, defaultCard, ok(`{"status":"ok","balance_seconds":18000}`))
	got, err := api.client(t).Buy(context.Background(), "300min", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seconds != 18000 || got.Minutes != 300 {
		t.Errorf("purchase = %+v", got)
	}
	if n := len(api.all()); n != 2 {
		t.Fatalf("%d requests, want the cards read and the purchase and nothing after it: %v", n, api.all())
	}
	purchase := nth(t, api.to(purchaseRoute), 0)
	body := purchaseBody(t, purchase)
	key, _ := body["idempotency_key"].(string)
	if body["pack"] != "300min" || body["quick_payment_method_id"] != "pm_1" || body["policy_ack"] != false || len(key) != 36 {
		t.Errorf("body = %v", body)
	}
	if purchase.header.Get("Authorization") != "Bearer "+testToken {
		t.Errorf("Authorization = %q", purchase.header.Get("Authorization"))
	}
}

func TestBuySendsPolicyAckOnlyWhenTheNoticeWasAcknowledged(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		api := buyServer(t, defaultCard, ok(`{"status":"ok","balance_seconds":600}`))
		if _, err := api.client(t).Buy(context.Background(), "45min", &BuyOptions{AcknowledgeNoRefund: acknowledged}); err != nil {
			t.Fatal(err)
		}
		if got := purchaseBody(t, nth(t, api.to(purchaseRoute), 0))["policy_ack"]; got != acknowledged {
			t.Errorf("acknowledged=%v: policy_ack = %v", acknowledged, got)
		}
	}
}

func TestBuyMinutesTruncateTowardZero(t *testing.T) {
	for seconds, want := range map[int]int{119: 1, 59: 0, 0: 0, -59: 0, -90: -1, 18000: 300} {
		api := buyServer(t, defaultCard, ok(`{"status":"ok","balance_seconds":`+itoa(seconds)+`}`))
		got, err := api.client(t).Buy(context.Background(), "300min", nil)
		if err != nil || got.Seconds != seconds || got.Minutes != want {
			t.Errorf("balance %d: %+v, %v; want %d minutes", seconds, got, err, want)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestBuyPicksTheDefaultThenTheOnlyEligibleCardThenRefusesBeforeAnyPurchase(t *testing.T) {
	pick := func(t *testing.T, cards string) (*routeServer, string, error) {
		t.Helper()
		api := buyServer(t, cards, ok(`{"status":"ok","balance_seconds":600}`))
		_, err := api.client(t).Buy(context.Background(), "45min", nil)
		used := ""
		if p := api.to(purchaseRoute); len(p) > 0 {
			used, _ = purchaseBody(t, p[0])["quick_payment_method_id"].(string)
		}
		return api, used, err
	}
	if _, used, err := pick(t, cardsBody("pm_b", card("pm_a", false, true), card("pm_b", true, true))); err != nil || used != "pm_b" {
		t.Errorf("default: used %q, %v", used, err)
	}
	if _, used, err := pick(t, cardsBody("", card("pm_x", false, false), card("pm_y", false, true))); err != nil || used != "pm_y" {
		t.Errorf("single eligible: used %q, %v", used, err)
	}
	for name, tc := range map[string]struct {
		cards, code, hint string
	}{
		"no cards":        {cardsBody(""), "no_default_card", "add-card link"},
		"none eligible":   {cardsBody("", card("pm_x", false, false)), "no_default_card", "add-card link"},
		"several, no one": {cardsBody("", card("pm_x", false, true), card("pm_y", false, true)), "default_card_required", "Set a default card"},
	} {
		api, used, err := pick(t, tc.cards)
		e := wantCode(t, err, KindPaymentMethodRequired, tc.code)
		if used != "" || len(api.to(purchaseRoute)) != 0 || e.IdempotencyKey != "" || !strings.Contains(e.Hint, tc.hint) || e.Status != 0 {
			t.Errorf("%s: used %q, %d purchases, key %q, hint %q, status %d", name, used, len(api.to(purchaseRoute)), e.IdempotencyKey, e.Hint, e.Status)
		}
	}
}

func TestAPurchaseIsRetriedOnceOnA502WithTheSameKey(t *testing.T) {
	api := buyServer(t, defaultCard, step{status: 502, body: "bad gateway"}, ok(`{"status":"ok","balance_seconds":18000}`))
	if _, err := api.client(t).Buy(context.Background(), "300min", nil); err != nil {
		t.Fatal(err)
	}
	sent := api.to(purchaseRoute)
	if len(sent) != 2 || purchaseBody(t, sent[0])["idempotency_key"] != purchaseBody(t, sent[1])["idempotency_key"] {
		t.Errorf("purchases = %v", sent)
	}
}

func TestEveryErrorAfterThePurchaseWasSentCarriesTheKeyAndNeverRetriesTwice(t *testing.T) {
	for name, tc := range map[string]struct {
		steps    []step
		purchase int
		check    func(*testing.T, *Error)
	}{
		"502 twice": {[]step{{status: 502, body: "x"}}, 2, func(t *testing.T, e *Error) {
			if e.Kind != KindAPI || e.Code != "http_502" || e.Status != 502 {
				t.Errorf("error %+v", e)
			}
		}},
		"connection lost twice": {[]step{{drop: true}}, 2, func(t *testing.T, e *Error) {
			if e.Kind != KindNetwork || e.Code != "network_error" || e.Status != 0 {
				t.Errorf("error %+v", e)
			}
		}},
		"4021": {[]step{{status: 403, body: jsonError("pat_purchase_requires_app", 4021)}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindPermissionDenied || e.Code != "pat_purchase_requires_app" || e.Status != 403 || e.NumericCode != 4021 || !strings.Contains(e.Hint, "Buy this pack in the Hushscript app.") {
				t.Errorf("error %+v", e)
			}
		}},
		"4023": {[]step{{status: 403, body: jsonError("pat_purchase_requires_authentication", 4023)}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindPermissionDenied || e.Code != "pat_purchase_requires_authentication" || e.Status != 403 || e.NumericCode != 4023 || !strings.Contains(e.Hint, "Your bank wants to confirm this payment.") {
				t.Errorf("error %+v", e)
			}
		}},
		"declined card": {[]step{{status: 402, body: jsonError("insufficient_balance", 3001)}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindInsufficientBalance || e.Code != "insufficient_balance" || e.NumericCode != 3001 || e.Status != 402 {
				t.Errorf("error %+v", e)
			}
		}},
		"429": {[]step{{status: 429, body: jsonError("agent_purchase_capped", 4038), headers: map[string]string{"Retry-After": "3600"}}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindRateLimit || e.Code != "agent_purchase_capped" || e.Status != 429 || e.RetryAfter != time.Hour {
				t.Errorf("error %+v", e)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			api := buyServer(t, defaultCard, tc.steps...)
			_, err := api.client(t).Buy(context.Background(), "300min", nil)
			e := asError(t, err)
			sent := api.to(purchaseRoute)
			if len(sent) != tc.purchase {
				t.Errorf("%d purchase requests, want %d", len(sent), tc.purchase)
			}
			if want, _ := purchaseBody(t, nth(t, sent, 0))["idempotency_key"].(string); e.IdempotencyKey != want || want == "" {
				t.Errorf("error key %q, sent key %q", e.IdempotencyKey, want)
			}
			tc.check(t, e)
		})
	}
}

func TestAnUnusablePurchaseAnswerIsBadResponseWithItsStatusAndTheKey(t *testing.T) {
	for name, answer := range map[string]step{
		"empty object":              {status: 200, body: `{}`},
		"no status":                 {status: 200, body: `{"balance_seconds":600}`},
		"status not ok":             {status: 200, body: `{"status":"pending","balance_seconds":600}`},
		"no balance":                {status: 200, body: `{"status":"ok"}`},
		"fractional balance":        {status: 200, body: `{"status":"ok","balance_seconds":600.5}`},
		"string balance":            {status: 200, body: `{"status":"ok","balance_seconds":"600"}`},
		"null balance":              {status: 200, body: `{"status":"ok","balance_seconds":null}`},
		"truncated":                 {status: 200, body: `{"status":"ok","bal`},
		"201 without a usable body": {status: 201, body: `{"status":"ok"}`},
		"204":                       {status: 204},
	} {
		t.Run(name, func(t *testing.T) {
			api := buyServer(t, defaultCard, answer)
			_, err := api.client(t).Buy(context.Background(), "300min", nil)
			e := wantCode(t, err, KindAPI, "bad_response")
			if e.Status != answer.status || e.IdempotencyKey == "" || len(api.to(purchaseRoute)) != 1 {
				t.Errorf("status %d, key %q, %d purchases", e.Status, e.IdempotencyKey, len(api.to(purchaseRoute)))
			}
			if e.IdempotencyKey != purchaseBody(t, nth(t, api.to(purchaseRoute), 0))["idempotency_key"] {
				t.Errorf("the key on the error is not the one sent")
			}
		})
	}
}

func TestACallerKeyIsUsedAsGivenAndCheckedBeforeAnyRequest(t *testing.T) {
	api := buyServer(t, defaultCard, ok(`{"status":"ok","balance_seconds":600}`))
	if _, err := api.client(t).Buy(context.Background(), "45min", &BuyOptions{IdempotencyKey: "A-b_9"}); err != nil {
		t.Fatal(err)
	}
	if got := purchaseBody(t, nth(t, api.to(purchaseRoute), 0))["idempotency_key"]; got != "A-b_9" {
		t.Errorf("key = %v", got)
	}
	for name, key := range map[string]string{
		"too long": strings.Repeat("a", 129), "space": "has space", "dot": "a.b", "non ASCII": "clé", "slash": "a/b", "colon": "a:b",
	} {
		quiet := buyServer(t, defaultCard, ok(`{}`))
		_, err := quiet.client(t).Buy(context.Background(), "45min", &BuyOptions{IdempotencyKey: key})
		e := wantCode(t, err, KindPreflight, "invalid_option")
		if len(quiet.all()) != 0 || e.Status != 0 {
			t.Errorf("%s: %d requests, status %d", name, len(quiet.all()), e.Status)
		}
	}
	long := strings.Repeat("k", 128)
	if _, err := buyServer(t, defaultCard, ok(`{"status":"ok","balance_seconds":1}`)).client(t).Buy(context.Background(), "45min", &BuyOptions{IdempotencyKey: long}); err != nil {
		t.Errorf("a 128 character key: %v", err)
	}
}

func TestAnErrorBeforeThePurchaseWasSentCarriesNoKeyAndNamesTheScope(t *testing.T) {
	api := newRouteServer(t, func(recorded, int) step {
		return step{status: 403, body: jsonError("pat_scope_missing", 2023)}
	})
	_, err := api.client(t).Buy(context.Background(), "45min", nil)
	e := wantCode(t, err, KindPermissionDenied, "pat_scope_missing")
	if e.IdempotencyKey != "" || len(api.to(purchaseRoute)) != 0 {
		t.Errorf("key %q, %d purchases", e.IdempotencyKey, len(api.to(purchaseRoute)))
	}
	for _, want := range []string{`"billing" permission`, `"Let this token buy minutes"`, `"Let connected apps buy minutes"`, "Account → MCP"} {
		if !strings.Contains(e.Hint, want) {
			t.Errorf("hint %q lacks %q", e.Hint, want)
		}
	}
}

func TestACancelledPurchaseIsAnAbortErrorWithTheKeyAndNothingAfterASuccessCanFail(t *testing.T) {
	api := buyServer(t, defaultCard, step{status: 200, body: `{"status":"ok","balance_seconds":600}`, delay: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := api.client(t).Buy(ctx, "45min", nil)
	e := wantCode(t, err, KindAbort, "aborted")
	if e.IdempotencyKey == "" {
		t.Error("an aborted purchase must carry its key")
	}

	// A context cancelled only after the answer arrived does not turn a charged purchase into an error.
	late, lateCancel := context.WithCancel(context.Background())
	quick := buyServer(t, defaultCard, ok(`{"status":"ok","balance_seconds":600}`))
	got, err := quick.client(t).Buy(late, "45min", nil)
	lateCancel()
	if err != nil || got.Seconds != 600 {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestEveryErrorOfBuyIsAnSDKError(t *testing.T) {
	for _, answer := range []step{{status: 200, body: `[`}, {status: 500, body: ``}, {drop: true}} {
		api := buyServer(t, defaultCard, answer)
		_, err := api.client(t).Buy(context.Background(), "45min", nil)
		var sdk *Error
		if !errors.As(err, &sdk) {
			t.Errorf("%v (%T) is not a *hushscript.Error", err, err)
		}
	}
	_, err := (&Client{}).Buy(context.Background(), "45min", nil)
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
}

func TestPacksCarryThePriceTheAPIStatesAndAbsentAvailabilityMeansAvailable(t *testing.T) {
	api := newRouteServer(t, func(recorded, int) step {
		return ok(`{"packs":[{"id":"45min","seconds":2700,"amount":199,"currency":"usd","referral_bonus_seconds":0},` +
			`{"id":"6000min","seconds":360000,"amount":123456,"currency":"jpy","referral_bonus_seconds":650,"available":false},` +
			`{"id":"300min","seconds":18000,"amount":599,"currency":"eur","referral_bonus_seconds":0,"available":true}]}`)
	})
	packs, err := api.client(t).Billing.Packs(context.Background())
	if err != nil || len(packs) != 3 {
		t.Fatalf("%v, %v", packs, err)
	}
	if p := packs[0]; p.ID != "45min" || p.Seconds != 2700 || p.Minutes != 45 || p.Price != (Money{199, "usd", "$1.99"}) || p.BonusMinutes != 0 || !p.Available {
		t.Errorf("first = %+v", p)
	}
	if p := packs[1]; p.Minutes != 6000 || p.Price.Formatted != "¥123,456" || p.BonusMinutes != 10 || p.Available {
		t.Errorf("second = %+v", p)
	}
	if p := packs[2]; p.Price.Formatted != "€5.99" || !p.Available {
		t.Errorf("third = %+v", p)
	}
	bad := newRouteServer(t, func(recorded, int) step { return ok(`{}`) })
	_, err = bad.client(t).Billing.Packs(context.Background())
	_ = wantCode(t, err, KindAPI, "bad_response")
}

func TestCardsAndTheDefaultCardAndTheAddCardURL(t *testing.T) {
	api := newRouteServer(t, func(rec recorded, nth int) step {
		switch rec.method + " " + rec.path {
		case cardsRoute:
			return ok(cardsBody("pm_1", card("pm_1", true, true), strings.Replace(card("pm_2", false, false), `"email":null`, `"email":"a@b.c"`, 1)))
		case "POST /v1/billing/cards/default":
			return ok(`{"ok":true}`)
		case "POST /v1/billing/setup-session":
			if nth == 1 {
				return step{status: 502, body: "x"}
			}
			return ok(`{"checkout_url":"https://pay.example/setup","expires_at":1760000000,"session_id":"cs_1"}`)
		}
		return step{status: 404, body: jsonError("not_found", 4040)}
	})
	c := api.client(t)
	cards, err := c.Billing.Cards(context.Background())
	if err != nil || cards.DefaultID != "pm_1" || len(cards.Cards) != 2 {
		t.Fatalf("%+v, %v", cards, err)
	}
	if first := cards.Cards[0]; first.ID != "pm_1" || first.Brand != "visa" || first.Last4 != "4242" || first.ExpMonth != 4 || first.ExpYear != 2030 ||
		first.Email != "" || !first.IsDefault || !first.FastCheckoutEligible || first.Type != "card" {
		t.Errorf("first = %+v", first)
	}
	if cards.Cards[1].Email != "a@b.c" || cards.Cards[1].FastCheckoutEligible {
		t.Errorf("second = %+v", cards.Cards[1])
	}
	if setErr := c.Billing.SetDefaultCard(context.Background(), "pm_2"); setErr != nil {
		t.Fatal(setErr)
	}
	if got := nth(t, api.to("POST /v1/billing/cards/default"), 0).body; got != `{"payment_method_id":"pm_2"}` {
		t.Errorf("body = %s", got)
	}
	url, err := c.Billing.AddCardURL(context.Background())
	if err != nil || url != "https://pay.example/setup" {
		t.Errorf("url %q, %v", url, err)
	}
	sent := api.to("POST /v1/billing/setup-session")
	if len(sent) != 2 || sent[0].body != sent[1].body || !strings.Contains(sent[0].body, `"idempotency_key":"`) {
		t.Errorf("setup-session requests = %v: one retry with the same key", sent)
	}
}

func TestTransactionsLedgerAndHoldsPageThroughTheirCursors(t *testing.T) {
	api := newRouteServer(t, func(rec recorded, _ int) step {
		next := `"next_cursor":null`
		if rec.query == "" {
			next = `"next_cursor":"c2"`
		}
		switch rec.path {
		case "/v1/billing/transactions":
			return ok(`{"account_reference":"acc","transactions":[{"id":7,"seconds":18000,"amount_cents":599,"amount_refunded_cents":0,"currency":"usd","status":"succeeded",` +
				`"created_at":1760000000,"payment_method_type":"card","payment_method_brand":"visa","payment_method_last4":"4242","receipt_number":"HS-1","initiated_via":null}],` + next + `}`)
		case "/v1/billing/ledger":
			return ok(`{"entries":[{"id":3,"delta_seconds":-60,"kind":"settle","job_id":"j1","note":null,"created_at":1760000001}],` + next + `}`)
		case "/v1/billing/holds":
			return ok(`{"holds":[{"id":"j1","kind":"transcription","seconds":120,"created_at":1760000002}],` + next + `}`)
		}
		return step{status: 404, body: jsonError("not_found", 4040)}
	})
	c := api.client(t)
	transactions, err := drain(c.Billing.Transactions(context.Background()))
	if err != nil || len(transactions) != 2 {
		t.Fatalf("%v, %v", transactions, err)
	}
	if tr := transactions[0]; tr.ID != 7 || tr.Seconds != 18000 || tr.AmountCents != 599 || tr.Currency != "usd" || tr.Status != "succeeded" ||
		tr.CreatedAt.Unix() != 1760000000 || tr.PaymentMethodBrand != "visa" || tr.PaymentMethodLast4 != "4242" || tr.ReceiptNumber != "HS-1" || tr.InitiatedVia != "" {
		t.Errorf("transaction = %+v", tr)
	}
	entries, err := drain(c.Billing.Ledger(context.Background()))
	if err != nil || len(entries) != 2 || entries[0].DeltaSeconds != -60 || entries[0].Kind != "settle" || entries[0].JobID != "j1" || entries[0].Note != "" {
		t.Errorf("ledger = %+v, %v", entries, err)
	}
	holds, err := drain(c.Billing.Holds(context.Background()))
	if err != nil || len(holds) != 2 || holds[0].ID != "j1" || holds[0].Kind != "transcription" || holds[0].Seconds != 120 || holds[0].CreatedAt.Unix() != 1760000002 {
		t.Errorf("holds = %+v, %v", holds, err)
	}
	if got := api.to("GET /v1/billing/holds"); len(got) != 2 || got[1].query != "cursor=c2" {
		t.Errorf("holds requests = %v", got)
	}
}

func TestSetDefaultCardFailuresAreNotRetriedAfterSend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer step
		kind   ErrorKind
		code   string
		status int
	}{
		{"bad request", step{status: 400, body: jsonError("invalid_request", 1000)}, KindValidation, "invalid_request", 400},
		{"forbidden", step{status: 403, body: jsonError("forbidden", 2000)}, KindPermissionDenied, "forbidden", 403},
		{"not found", step{status: 404, body: jsonError("not_found", 4040)}, KindNotFound, "not_found", 404},
		{"unprocessable", step{status: 422, body: jsonError("invalid_request", 1000)}, KindValidation, "invalid_request", 422},
		{"lost answer", step{drop: true}, KindNetwork, "network_error", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return tc.answer })
			e := wantCode(t, srv.client(t).Billing.SetDefaultCard(context.Background(), "pm_1"), tc.kind, tc.code)
			if e.Status != tc.status || len(srv.all()) != 1 || len(srv.to("POST /v1/billing/cards/default")) != 1 {
				t.Errorf("status %d, requests %v", e.Status, srv.all())
			}
		})
	}
}

func TestAddCardURLFailuresRespectThePurchaseRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		status, requests int
		kind             ErrorKind
	}{
		{400, 1, KindValidation},
		{403, 1, KindPermissionDenied},
		{404, 1, KindNotFound},
		{422, 1, KindValidation},
		{500, 1, KindAPI},
		{503, 2, KindAPI},
	} {
		t.Run(itoa(tc.status), func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return step{status: tc.status, body: "unavailable"} })
			_, err := srv.client(t).Billing.AddCardURL(context.Background())
			e := wantCode(t, err, tc.kind, "http_"+itoa(tc.status))
			sent := srv.to("POST /v1/billing/setup-session")
			if e.Status != tc.status || len(sent) != tc.requests || len(srv.all()) != tc.requests {
				t.Fatalf("status %d, requests %v, want %d", e.Status, srv.all(), tc.requests)
			}
			if tc.requests == 2 && sent[0].body != sent[1].body {
				t.Errorf("retry changed the keyed body: %v", sent)
			}
		})
	}
}

func TestCanceledBillingCallsSendNothingAndCarryNoPurchaseKey(t *testing.T) {
	calls := map[string]func(context.Context, *Client) error{
		"Client.Buy": func(ctx context.Context, c *Client) error { _, err := c.Buy(ctx, "45min", nil); return err },
		"Billing.Buy": func(ctx context.Context, c *Client) error {
			_, err := c.Billing.Buy(ctx, "45min", &BuyOptions{IdempotencyKey: "caller-key"})
			return err
		},
		"Packs":          func(ctx context.Context, c *Client) error { _, err := c.Billing.Packs(ctx); return err },
		"Cards":          func(ctx context.Context, c *Client) error { _, err := c.Billing.Cards(ctx); return err },
		"SetDefaultCard": func(ctx context.Context, c *Client) error { return c.Billing.SetDefaultCard(ctx, "pm_1") },
		"AddCardURL":     func(ctx context.Context, c *Client) error { _, err := c.Billing.AddCardURL(ctx); return err },
		"Transactions": func(ctx context.Context, c *Client) error {
			for _, err := range c.Billing.Transactions(ctx) {
				return err
			}
			return nil
		},
		"Ledger": func(ctx context.Context, c *Client) error {
			for _, err := range c.Billing.Ledger(ctx) {
				return err
			}
			return nil
		},
		"Holds": func(ctx context.Context, c *Client) error {
			for _, err := range c.Billing.Holds(ctx) {
				return err
			}
			return nil
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			srv := newRouteServer(t, func(recorded, int) step { return step{status: 403, body: jsonError("forbidden", 2000)} })
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			e := wantCode(t, call(ctx, srv.client(t)), KindAbort, "aborted")
			if e.Status != 0 || e.IdempotencyKey != "" || len(srv.all()) != 0 || !errors.Is(e, context.Canceled) {
				t.Errorf("status %d, key %q, requests %v, cause %v", e.Status, e.IdempotencyKey, srv.all(), e.Unwrap())
			}
		})
	}
}

func nestedRaw(t *testing.T, model any) string {
	t.Helper()
	field := reflect.ValueOf(model).FieldByName("Raw")
	if !field.IsValid() {
		t.Fatalf("%T has no Raw field", model)
	}
	raw, ok := field.Interface().(json.RawMessage)
	if !ok {
		t.Fatalf("%T.Raw is %T, want json.RawMessage", model, field.Interface())
	}
	return string(raw)
}

func TestNestedAccountLimitsAndJobOptionsKeepTheirOwnRaw(t *testing.T) {
	for _, unknown := range []string{`null`, `""`, `"   "`, `"\t"`, `"\u2003"`, `false`, `0`, `[]`, `{"deeper":[1,null,"x"]}`} {
		t.Run(unknown, func(t *testing.T) {
			limits := `{"max_daily_seconds":9,"max_active_jobs":3,"active_jobs":1,"used_today_seconds":2,"future":` + unknown + `}`
			accountServer := newScriptedServer(t, step{status: 200, headers: map[string]string{"Content-Type": "application/json"}, body: `{"balance_seconds":90,"held_seconds":10,"card_on_file":true,"data_region":"eu","credits_expire_at":null,"limits":` + limits + `}`})
			account, err := newTestClient(t, accountServer.URL, newFakeClock()).Account(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if account.Limits.MaxDailySeconds != 9 || account.Limits.MaxActiveJobs != 3 || account.Limits.ActiveJobs != 1 || account.Limits.UsedTodaySeconds != 2 {
				t.Fatalf("limits = %+v", account.Limits)
			}
			if raw := nestedRaw(t, account.Limits); raw != limits {
				t.Errorf("limits Raw = %s, want %s", raw, limits)
			}
			options := `{"language_code":null,"speaker_detection":true,"translation_targets":["fr"],"future":` + unknown + `}`
			jobServer := newScriptedServer(t, step{status: 200, headers: map[string]string{"Content-Type": "application/json"}, body: `{"id":"job_1","state":"done","created_at":1,"options":` + options + `}`})
			job, err := newTestClient(t, jobServer.URL, newFakeClock()).Jobs.Get(context.Background(), "job_1")
			if err != nil {
				t.Fatal(err)
			}
			if job.Options.LanguageCode != "" || !job.Options.SpeakerDetection || strings.Join(job.Options.TranslationTargets, ",") != "fr" {
				t.Fatalf("options = %+v", job.Options)
			}
			if raw := nestedRaw(t, job.Options); raw != options {
				t.Errorf("options Raw = %s, want %s", raw, options)
			}
		})
	}
}

func TestTopLevelResultsKeepTheirUnknownFieldsInRaw(t *testing.T) {
	future := `"x_future":{"deeper":[1,null,"x"]}`
	account := `{"balance_seconds":90,"held_seconds":10,"card_on_file":true,"data_region":"eu","credits_expire_at":null,` +
		`"limits":{"max_daily_seconds":9,"max_active_jobs":3,"active_jobs":1,"used_today_seconds":2},` + future + `}`
	packA := `{"id":"45min","seconds":2700,"amount":199,"currency":"usd","referral_bonus_seconds":0,"x_a":1}`
	packB := `{"id":"300min","seconds":18000,"amount":599,"currency":"usd","referral_bonus_seconds":0,"x_b":"two"}`
	cardA, cardB := card("pm_1", true, true), card("pm_2", false, true)
	cardA, cardB = strings.Replace(cardA, `{`, `{"x_a":1,`, 1), strings.Replace(cardB, `{`, `{"x_b":"two",`, 1)
	cards := cardsBody("pm_1", cardA, cardB)
	cards = strings.Replace(cards, `{`, `{`+future+`,`, 1)
	purchase := `{"status":"ok","balance_seconds":600,` + future + `}`
	transactions := []string{
		`{"id":7,"seconds":18000,"amount_cents":599,"amount_refunded_cents":0,"currency":"usd","status":"succeeded","created_at":1760000000,"x_a":1}`,
		`{"id":8,"seconds":600,"amount_cents":99,"amount_refunded_cents":0,"currency":"usd","status":"succeeded","created_at":1760000001,"x_b":"two"}`,
	}
	entries := []string{
		`{"id":3,"delta_seconds":-60,"kind":"settle","job_id":"j1","note":null,"created_at":1760000001,"x_a":1}`,
		`{"id":4,"delta_seconds":60,"kind":"release","job_id":null,"note":"n","created_at":1760000002,"x_b":"two"}`,
	}
	holds := []string{
		`{"id":"j1","kind":"transcription","seconds":120,"created_at":1760000002,"x_a":1}`,
		`{"id":"j2","kind":"insights","seconds":30,"created_at":1760000003,"x_b":"two"}`,
	}
	jobOne := jobJSON("j1", "done", `,"x_a":1`)
	jobTwo := jobJSON("j2", "queued", `,"x_b":"two"`)
	api := newRouteServer(t, func(rec recorded, _ int) step {
		switch rec.method + " " + rec.path {
		case "GET /v1/account":
			return ok(account)
		case "GET /v1/jobs/j1":
			return ok(jobOne)
		case "GET /v1/jobs":
			return ok(`{"jobs":[` + jobOne + `,` + jobTwo + `],"next_cursor":null}`)
		case "GET /v1/billing/packs":
			return ok(`{"packs":[` + packA + `,` + packB + `]}`)
		case cardsRoute:
			return ok(cards)
		case purchaseRoute:
			return ok(purchase)
		case "GET /v1/billing/transactions":
			return ok(`{"account_reference":"acc","transactions":[` + strings.Join(transactions, ",") + `],"next_cursor":null}`)
		case "GET /v1/billing/ledger":
			return ok(`{"entries":[` + strings.Join(entries, ",") + `],"next_cursor":null}`)
		case "GET /v1/billing/holds":
			return ok(`{"holds":[` + strings.Join(holds, ",") + `],"next_cursor":null}`)
		}
		return step{status: 404, body: jsonError("not_found", 4040)}
	})
	client := api.client(t)
	ctx := context.Background()
	check := func(what string, model any, want string) {
		t.Helper()
		if got := nestedRaw(t, model); got != want {
			t.Errorf("%s Raw = %s, want %s", what, got, want)
		}
	}

	gotAccount, err := client.Account(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("Account", *gotAccount, account)
	balance, err := client.Balance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	check("Balance", *balance, account)
	gotJob, err := client.Jobs.Get(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	check("Job", *gotJob, jobOne)
	jobs, err := drain(client.Jobs.List(ctx, nil))
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %v, %v", jobs, err)
	}
	check("listed Job 1", jobs[0], jobOne)
	check("listed Job 2", jobs[1], jobTwo)
	packs, err := client.Billing.Packs(ctx)
	if err != nil || len(packs) != 2 {
		t.Fatalf("packs = %v, %v", packs, err)
	}
	check("Pack 1", packs[0], packA)
	check("Pack 2", packs[1], packB)
	gotCards, err := client.Billing.Cards(ctx)
	if err != nil || len(gotCards.Cards) != 2 {
		t.Fatalf("cards = %v, %v", gotCards, err)
	}
	check("Cards", *gotCards, cards)
	check("Card 1", gotCards.Cards[0], cardA)
	check("Card 2", gotCards.Cards[1], cardB)
	bought, err := client.Billing.Buy(ctx, "45min", nil)
	if err != nil {
		t.Fatal(err)
	}
	check("Purchase", *bought, purchase)
	gotTransactions, err := drain(client.Billing.Transactions(ctx))
	if err != nil || len(gotTransactions) != 2 {
		t.Fatalf("transactions = %v, %v", gotTransactions, err)
	}
	gotEntries, err := drain(client.Billing.Ledger(ctx))
	if err != nil || len(gotEntries) != 2 {
		t.Fatalf("entries = %v, %v", gotEntries, err)
	}
	gotHolds, err := drain(client.Billing.Holds(ctx))
	if err != nil || len(gotHolds) != 2 {
		t.Fatalf("holds = %v, %v", gotHolds, err)
	}
	for i := range 2 {
		check("Transaction", gotTransactions[i], transactions[i])
		check("LedgerEntry", gotEntries[i], entries[i])
		check("Hold", gotHolds[i], holds[i])
	}
}
