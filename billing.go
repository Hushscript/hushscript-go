package hushscript

import (
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

// Pack is a minute pack as the account can see it.
type Pack struct {
	// ID is 45min, 300min, 900min, 1800min or 6000min.
	ID      string
	Seconds int
	// Minutes is Seconds / 60, rounded toward zero.
	Minutes int
	// Price is in the account's own currency, as the API states it: never hard-code one.
	Price Money
	// BonusMinutes is what an available referral bonus would add to this purchase; 0 when none.
	BonusMinutes int
	// Available is false only for an agent account's tier-locked pack.
	Available bool
	// Raw is the pack as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// Card is a saved payment method.
type Card struct {
	ID    string
	Type  string
	Brand string
	Last4 string
	// ExpMonth and ExpYear are 0 when the method has no expiry.
	ExpMonth int
	ExpYear  int
	// Email is "" when the method has none.
	Email     string
	IsDefault bool
	// FastCheckoutEligible says whether Buy may charge it without the buyer present.
	FastCheckoutEligible bool
	// Raw is the method as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// Cards is the saved payment methods and which one is the default.
type Cards struct {
	// DefaultID is "" when there is no default card.
	DefaultID string
	Cards     []Card
	// Raw is the answer as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// Purchase is what a purchase left the account with, read from the purchase answer alone.
type Purchase struct {
	// Seconds is the balance after the purchase settled: spendable now, holds already deducted.
	Seconds int
	// Minutes is Seconds / 60, rounded toward zero.
	Minutes int
	// Raw is the purchase answer as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// BuyOptions are the options of Buy. The zero value buys with a fresh idempotency key and does not
// acknowledge the notice.
type BuyOptions struct {
	// AcknowledgeNoRefund records that the no-refund, instant-delivery notice was shown to the buyer.
	// It is evidence for disputes and never gates the charge: without it Buy still goes ahead.
	AcknowledgeNoRefund bool
	// IdempotencyKey is 1 to 128 characters from A-Z, a-z, 0-9, _ and -. Left empty, Buy mints one.
	// Pass the key an error carried to ask again without any chance of a second charge.
	IdempotencyKey string
}

// Transaction is one purchase, as the API reports it.
type Transaction struct {
	ID                  int
	Seconds             int
	AmountCents         int
	AmountRefundedCents int
	Currency            string
	Status              string
	CreatedAt           time.Time
	PaymentMethodType   string
	PaymentMethodBrand  string
	PaymentMethodLast4  string
	// ReceiptNumber is the public receipt reference; "" when there is none.
	ReceiptNumber string
	InitiatedVia  string
	// Raw is the row as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// LedgerEntry is one movement of the credit balance.
type LedgerEntry struct {
	ID           int
	DeltaSeconds int
	Kind         string
	// JobID and Note are "" when the entry has none.
	JobID     string
	Note      string
	CreatedAt time.Time
	// Raw is the entry as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// Hold is seconds reserved by work still running; they are already deducted from the balance.
type Hold struct {
	ID string
	// Kind is transcription, translation, insights or qa; a kind the SDK does not know yet passes through.
	Kind      string
	Seconds   int
	CreatedAt time.Time
	// Raw is the row as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// BillingService is the friendly layer over the billing endpoints: Client.Billing.
type BillingService struct {
	core *core
}

const buyRoute = "POST /v1/billing/purchase"

// Packs lists the minute packs the account can buy, priced in its own currency. It needs the billing scope.
func (b BillingService) Packs(ctx context.Context) ([]Pack, error) {
	if b.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	core := b.core
	const route = "GET /v1/billing/packs"
	response, err := core.api.ListPacksWithResponse(ctx)
	if err != nil {
		return nil, (&annotator{core: core}).annotate(ctx, core.readFailed(err, route))
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "packs"); failure != nil {
		return nil, failure
	}
	if !rowsHave(response.Body, "packs", "id", "seconds", "amount", "currency", "referral_bonus_seconds") {
		return nil, core.badResponse(route, response.StatusCode())
	}
	rows, raws, _, ok := listRows[generated.Pack](response.Body, "packs")
	if !ok {
		return nil, core.badResponse(route, response.StatusCode())
	}
	packs := make([]Pack, len(rows))
	for i, raw := range rows {
		packs[i] = Pack{
			ID: raw.Id, Seconds: raw.Seconds, Minutes: raw.Seconds / secondsPerMinute, Price: newMoney(raw.Amount, raw.Currency),
			BonusMinutes: raw.ReferralBonusSeconds / secondsPerMinute, Available: raw.Available == nil || *raw.Available, Raw: raws[i],
		}
	}
	return packs, nil
}

func (c *core) savedCards(ctx context.Context) (*Cards, error) {
	const route = "GET /v1/billing/cards"
	response, err := c.api.ListSavedCardsWithResponse(ctx)
	if err != nil {
		return nil, c.readFailed(err, route)
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := c.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "methods"); failure != nil {
		return nil, failure
	}
	if !rowsHave(response.Body, "methods", "id", "type", "brand", "last4", "exp_month", "exp_year", "is_default", "fast_checkout_eligible") {
		return nil, c.badResponse(route, response.StatusCode())
	}
	rows, raws, _, ok := listRows[generated.Card](response.Body, "methods")
	if !ok {
		return nil, c.badResponse(route, response.StatusCode())
	}
	cards := &Cards{DefaultID: valueOf(parsed.DefaultId), Cards: make([]Card, len(rows)), Raw: bytes.Clone(response.Body)}
	for i, raw := range rows {
		cards.Cards[i] = Card{
			ID: raw.Id, Type: raw.Type, Brand: raw.Brand, Last4: raw.Last4, ExpMonth: raw.ExpMonth, ExpYear: raw.ExpYear,
			Email: valueOf(raw.Email), IsDefault: raw.IsDefault, FastCheckoutEligible: raw.FastCheckoutEligible, Raw: raws[i],
		}
	}
	return cards, nil
}

// Cards lists the saved payment methods and the default one.
func (b BillingService) Cards(ctx context.Context) (*Cards, error) {
	if b.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	cards, err := b.core.savedCards(ctx)
	if err != nil {
		return nil, (&annotator{core: b.core}).annotate(ctx, err)
	}
	return cards, nil
}

// SetDefaultCard makes a saved card the default one.
func (b BillingService) SetDefaultCard(ctx context.Context, cardID string) error {
	if b.core == nil {
		return newConfigError(notBuiltMessage)
	}
	_, err := b.core.api.SetDefaultCardWithResponse(ctx, generated.SetDefaultCardJSONRequestBody{PaymentMethodId: cardID})
	if err != nil {
		return (&annotator{core: b.core}).annotate(ctx, b.core.readFailed(err, "POST /v1/billing/cards/default"))
	}
	return nil
}

// AddCardURL returns the hosted page where a card is saved; open it in a browser. The request carries
// a key, so its one retry after a 502 lands on the same page.
func (b BillingService) AddCardURL(ctx context.Context) (string, error) {
	if b.core == nil {
		return "", newConfigError(notBuiltMessage)
	}
	core := b.core
	const route = "POST /v1/billing/setup-session"
	key := newIdempotencyKey()
	response, err := core.api.CreateSetupSessionWithResponse(ctx, generated.CreateSetupSessionJSONRequestBody{IdempotencyKey: &key})
	if err != nil {
		return "", (&annotator{core: core}).annotate(ctx, core.readFailed(err, route))
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "checkout_url"); failure != nil {
		return "", failure
	}
	return parsed.CheckoutUrl, nil
}

// maxPurchaseKey is the longest idempotency key a purchase takes (spec 7.3).
const maxPurchaseKey = 128

// validPurchaseKey is spec 7.3: 1 to 128 characters from A-Z, a-z, 0-9, _ and -.
func validPurchaseKey(key string) bool {
	if key == "" || len(key) > maxPurchaseKey {
		return false
	}
	for i := range len(key) {
		c := key[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// chooseCard is the card Buy charges: the default, else the only one that can be charged.
func (c *core) chooseCard(cards *Cards) (string, *Error) {
	if cards.DefaultID != "" {
		return cards.DefaultID, nil
	}
	var eligible []string
	for i := range cards.Cards {
		if card := &cards.Cards[i]; card.FastCheckoutEligible {
			eligible = append(eligible, card.ID)
		}
	}
	switch len(eligible) {
	case 1:
		return eligible[0], nil
	case 0:
		return "", newSDKError(KindPaymentMethodRequired, "no_default_card", "The account has no saved card that can be charged.", nil, c.cfg.redactor(), hintInputs{})
	default:
		return "", newSDKError(KindPaymentMethodRequired, "default_card_required",
			"The account has no default card and several that can be charged.", nil, c.cfg.redactor(), hintInputs{})
	}
}

// Buy buys a pack with the default card, or the only card that can be charged when there is no
// default, and returns the balance the purchase answer reports. It makes no request after the charge,
// so nothing can fail once the money has moved. Every error raised after the purchase request was sent
// carries IdempotencyKey; pass it back in BuyOptions to ask again with no chance of a second charge. A
// nil opts uses the defaults.
func (b BillingService) Buy(ctx context.Context, pack string, opts *BuyOptions) (*Purchase, error) {
	if b.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	core := b.core
	if opts == nil {
		opts = &BuyOptions{}
	}
	key := opts.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	} else if !validPurchaseKey(key) {
		return nil, invalidOption(core.cfg.redactor(), "IdempotencyKey must be 1 to 128 characters from A-Z, a-z, 0-9, _ and -.", nil)
	}
	ann := &annotator{core: core}
	cards, err := core.savedCards(ctx)
	if err != nil {
		return nil, ann.annotate(ctx, err)
	}
	card, choiceErr := core.chooseCard(cards)
	if choiceErr != nil {
		return nil, choiceErr
	}
	ann.key = key // from here on the purchase may have reached the server
	purchase, err := core.purchase(ctx, generated.BuyMinutesJSONRequestBody{
		Pack: pack, IdempotencyKey: key, QuickPaymentMethodId: card, PolicyAck: &opts.AcknowledgeNoRefund,
	})
	if err != nil {
		return nil, ann.annotate(ctx, err)
	}
	return purchase, nil
}

func (c *core) purchase(ctx context.Context, body generated.BuyMinutesJSONRequestBody) (*Purchase, error) {
	response, err := c.api.BuyMinutesWithResponse(ctx, body)
	if err != nil {
		return nil, c.readFailed(err, buyRoute)
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := c.unusable(buyRoute, response.HTTPResponse, parsed != nil, response.Body, "status", "balance_seconds"); failure != nil {
		return nil, failure
	}
	if parsed.Status != generated.Ok {
		return nil, c.badResponse(buyRoute, response.StatusCode())
	}
	seconds := parsed.BalanceSeconds
	return &Purchase{Seconds: seconds, Minutes: seconds / secondsPerMinute, Raw: bytes.Clone(response.Body)}, nil
}

// Buy buys a pack; it is Billing.Buy.
func (c Client) Buy(ctx context.Context, pack string, opts *BuyOptions) (*Purchase, error) {
	return c.Billing.Buy(ctx, pack, opts)
}

// Transactions walks the purchases, newest first. The iterator yields an error once and then ends.
func (b BillingService) Transactions(ctx context.Context) iter.Seq2[Transaction, error] {
	if b.core == nil {
		return failedSeq[Transaction](newConfigError(notBuiltMessage))
	}
	core := b.core
	return paginate(ctx, core, func(ctx context.Context, cursor string) (Page[Transaction], error) {
		const route = "GET /v1/billing/transactions"
		params := &generated.ListTransactionsParams{}
		if cursor != "" {
			params.Cursor = &cursor
		}
		response, err := core.api.ListTransactionsWithResponse(ctx, params)
		if err != nil {
			return Page[Transaction]{}, core.readFailed(err, route)
		}
		parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
		if failure := core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "transactions"); failure != nil {
			return Page[Transaction]{}, failure
		}
		if !rowsHave(response.Body, "transactions", "id", "seconds", "amount_cents", "amount_refunded_cents", "currency", "status", "created_at") {
			return Page[Transaction]{}, core.badResponse(route, response.StatusCode())
		}
		rows, raws, cursor, ok := listRows[generated.Transaction](response.Body, "transactions")
		if !ok {
			return Page[Transaction]{}, core.badResponse(route, response.StatusCode())
		}
		page := Page[Transaction]{NextCursor: cursor, Items: make([]Transaction, len(rows))}
		for i, row := range rows {
			page.Items[i] = Transaction{
				ID: row.Id, Seconds: row.Seconds, AmountCents: row.AmountCents, AmountRefundedCents: row.AmountRefundedCents,
				Currency: row.Currency, Status: row.Status, CreatedAt: unixTime(int64(row.CreatedAt)),
				PaymentMethodType: valueOf(row.PaymentMethodType), PaymentMethodBrand: valueOf(row.PaymentMethodBrand),
				PaymentMethodLast4: valueOf(row.PaymentMethodLast4), ReceiptNumber: valueOf(row.ReceiptNumber), InitiatedVia: valueOf(row.InitiatedVia), Raw: raws[i],
			}
		}
		return page, nil
	})
}

// Ledger walks every movement of the credit balance, newest first: holds, settles, releases, purchases
// and refunds. The iterator is as in Transactions.
func (b BillingService) Ledger(ctx context.Context) iter.Seq2[LedgerEntry, error] {
	if b.core == nil {
		return failedSeq[LedgerEntry](newConfigError(notBuiltMessage))
	}
	core := b.core
	return paginate(ctx, core, func(ctx context.Context, cursor string) (Page[LedgerEntry], error) {
		const route = "GET /v1/billing/ledger"
		params := &generated.ListCreditLedgerParams{}
		if cursor != "" {
			params.Cursor = &cursor
		}
		response, err := core.api.ListCreditLedgerWithResponse(ctx, params)
		if err != nil {
			return Page[LedgerEntry]{}, core.readFailed(err, route)
		}
		parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
		if failure := core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "entries"); failure != nil {
			return Page[LedgerEntry]{}, failure
		}
		if !rowsHave(response.Body, "entries", "id", "delta_seconds", "kind", "created_at") {
			return Page[LedgerEntry]{}, core.badResponse(route, response.StatusCode())
		}
		rows, raws, cursor, ok := listRows[generated.CreditLedgerEntry](response.Body, "entries")
		if !ok {
			return Page[LedgerEntry]{}, core.badResponse(route, response.StatusCode())
		}
		page := Page[LedgerEntry]{NextCursor: cursor, Items: make([]LedgerEntry, len(rows))}
		for i, row := range rows {
			page.Items[i] = LedgerEntry{
				ID: row.Id, DeltaSeconds: row.DeltaSeconds, Kind: row.Kind, JobID: valueOf(row.JobId), Note: valueOf(row.Note),
				CreatedAt: unixTime(int64(row.CreatedAt)), Raw: raws[i],
			}
		}
		return page, nil
	})
}

// Holds walks the seconds reserved by work in flight, newest first. The iterator is as in Transactions.
func (b BillingService) Holds(ctx context.Context) iter.Seq2[Hold, error] {
	if b.core == nil {
		return failedSeq[Hold](newConfigError(notBuiltMessage))
	}
	core := b.core
	return paginate(ctx, core, func(ctx context.Context, cursor string) (Page[Hold], error) {
		const route = "GET /v1/billing/holds"
		params := &generated.ListHoldsParams{}
		if cursor != "" {
			params.Cursor = &cursor
		}
		response, err := core.api.ListHoldsWithResponse(ctx, params)
		if err != nil {
			return Page[Hold]{}, core.readFailed(err, route)
		}
		parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
		if failure := core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "holds"); failure != nil {
			return Page[Hold]{}, failure
		}
		if !rowsHave(response.Body, "holds", "id", "kind", "seconds", "created_at") {
			return Page[Hold]{}, core.badResponse(route, response.StatusCode())
		}
		rows, raws, cursor, ok := listRows[generated.ActiveHold](response.Body, "holds")
		if !ok {
			return Page[Hold]{}, core.badResponse(route, response.StatusCode())
		}
		page := Page[Hold]{NextCursor: cursor, Items: make([]Hold, len(rows))}
		for i, row := range rows {
			page.Items[i] = Hold{ID: row.Id, Kind: string(row.Kind), Seconds: row.Seconds, CreatedAt: unixTime(int64(row.CreatedAt)), Raw: raws[i]}
		}
		return page, nil
	})
}
