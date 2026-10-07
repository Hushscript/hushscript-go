package hushscript

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

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
		for key, value := range current.headers {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/json")
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
	purchase := api.to(purchaseRoute)[0]
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
		if got := purchaseBody(t, api.to(purchaseRoute)[0])["policy_ack"]; got != acknowledged {
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
		"no cards":        {cardsBody(""), "no_default_card", "hs.billing.addCardUrl()"},
		"none eligible":   {cardsBody("", card("pm_x", false, false)), "no_default_card", "hs.billing.addCardUrl()"},
		"several, no one": {cardsBody("", card("pm_x", false, true), card("pm_y", false, true)), "default_card_required", "hs.billing.setDefaultCard(id)"},
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
			if e.Kind != KindAPI || e.Status != 502 {
				t.Errorf("error %+v", e)
			}
		}},
		"connection lost twice": {[]step{{drop: true}}, 2, func(t *testing.T, e *Error) {
			if e.Kind != KindNetwork {
				t.Errorf("error %+v", e)
			}
		}},
		"4021": {[]step{{status: 403, body: jsonError("pat_purchase_requires_app", 4021)}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindPermissionDenied || e.NumericCode != 4021 || !strings.Contains(e.Hint, "Buy this pack in the Hushscript app.") {
				t.Errorf("error %+v", e)
			}
		}},
		"4023": {[]step{{status: 403, body: jsonError("pat_purchase_requires_authentication", 4023)}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindPermissionDenied || e.NumericCode != 4023 || !strings.Contains(e.Hint, "Your bank wants to confirm this payment.") {
				t.Errorf("error %+v", e)
			}
		}},
		"declined card": {[]step{{status: 402, body: jsonError("insufficient_balance", 3001)}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindInsufficientBalance {
				t.Errorf("error %+v", e)
			}
		}},
		"429": {[]step{{status: 429, body: jsonError("agent_purchase_capped", 4038), headers: map[string]string{"Retry-After": "3600"}}}, 1, func(t *testing.T, e *Error) {
			if e.Kind != KindRateLimit || e.RetryAfter != time.Hour {
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
			if want, _ := purchaseBody(t, sent[0])["idempotency_key"].(string); e.IdempotencyKey != want || want == "" {
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
			if e.IdempotencyKey != purchaseBody(t, api.to(purchaseRoute)[0])["idempotency_key"] {
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
	if got := purchaseBody(t, api.to(purchaseRoute)[0])["idempotency_key"]; got != "A-b_9" {
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
	if got := api.to("POST /v1/billing/cards/default")[0].body; got != `{"payment_method_id":"pm_2"}` {
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
