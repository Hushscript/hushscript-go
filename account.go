package hushscript

import (
	"bytes"
	"context"
	"encoding/json"
	"time"
)

// UploadLimits are the caller's current upload admission limits.
type UploadLimits struct {
	// MaxDailySeconds is the daily transcription cap, in seconds.
	MaxDailySeconds int
	// MaxActiveJobs is the most pipeline jobs in flight at once.
	MaxActiveJobs int
	// ActiveJobs is the pipeline jobs in flight now.
	ActiveJobs int
	// UsedTodaySeconds is the billable seconds used today.
	UsedTodaySeconds int
	// Raw is the limits object as the API sent it, unknown fields included.
	Raw json.RawMessage
}

// Account is the account: balance, limits, card and region (spec 6.3).
type Account struct {
	// BalanceSeconds is net of holds and clawback debt; it may be negative.
	BalanceSeconds int
	// HeldSeconds is committed to in-flight jobs and already deducted from BalanceSeconds.
	HeldSeconds int
	// CreditsExpireAt is when the balance expires; nil while no expiry is armed.
	CreditsExpireAt *time.Time
	CardOnFile      bool
	// DataRegion is for example us, eu, apac or oc.
	DataRegion string
	Limits     UploadLimits
	// PurchasesRemainingInWindow is set for agent accounts only; nil otherwise.
	PurchasesRemainingInWindow *int
}

// Balance is what an account can spend.
type Balance struct {
	// Seconds is spendable now and may be negative (clawback debt). The seconds held for in-flight jobs
	// are already deducted: it is not "balance minus held".
	Seconds int
	// Minutes is Seconds / 60, rounded toward zero.
	Minutes int
	// HeldSeconds is committed to in-flight jobs and already deducted from Seconds.
	HeldSeconds int
	// ExpiresAt is when the balance expires; nil while no expiry is armed.
	ExpiresAt *time.Time
	// PurchasesRemaining is set for agent accounts only; nil otherwise.
	PurchasesRemaining *int
}

type accountBody struct {
	BalanceSeconds             int        `json:"balance_seconds"`
	HeldSeconds                int        `json:"held_seconds"`
	CreditsExpireAt            *int64     `json:"credits_expire_at"`
	CardOnFile                 bool       `json:"card_on_file"`
	DataRegion                 string     `json:"data_region"`
	PurchasesRemainingInWindow *int       `json:"purchases_remaining_in_window"`
	Limits                     limitsBody `json:"limits"`
}

type limitsBody struct {
	MaxDailySeconds  int `json:"max_daily_seconds"`
	MaxActiveJobs    int `json:"max_active_jobs"`
	ActiveJobs       int `json:"active_jobs"`
	UsedTodaySeconds int `json:"used_today_seconds"`
	raw              json.RawMessage
}

func (l *limitsBody) UnmarshalJSON(data []byte) error {
	type fields limitsBody
	return decodeKeepingRaw(data, (*fields)(l), &l.raw)
}

func (l limitsBody) limits() UploadLimits {
	return UploadLimits{
		MaxDailySeconds: l.MaxDailySeconds, MaxActiveJobs: l.MaxActiveJobs, ActiveJobs: l.ActiveJobs,
		UsedTodaySeconds: l.UsedTodaySeconds, Raw: l.raw,
	}
}

// decodeKeepingRaw decodes data into the typed fields and keeps the object's own bytes beside them, so
// a key the API adds later stays reachable.
func decodeKeepingRaw[T any](data []byte, into *T, raw *json.RawMessage) error {
	if err := json.Unmarshal(data, into); err != nil {
		return err
	}
	*raw = bytes.Clone(data)
	return nil
}

func (a accountBody) expiry() *time.Time {
	if a.CreditsExpireAt == nil {
		return nil
	}
	at := unixTime(*a.CreditsExpireAt)
	return &at
}

// readAccount is GET /v1/account, read with the fields the friendly layer relies on.
func (c *core) readAccount(ctx context.Context, required ...string) (accountBody, error) {
	const route = "GET /v1/account"
	response, err := c.api.GetAccountWithResponse(ctx)
	if err != nil {
		return accountBody{}, c.readFailed(err, route)
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := c.unusable(route, response.HTTPResponse, parsed != nil, response.Body, required...); failure != nil {
		return accountBody{}, failure
	}
	var body accountBody
	if json.Unmarshal(response.Body, &body) != nil {
		return accountBody{}, c.badResponse(route, response.StatusCode())
	}
	return body, nil
}

func (c *core) balance(ctx context.Context) (*Balance, error) {
	body, err := c.readAccount(ctx, "balance_seconds", "held_seconds")
	if err != nil {
		return nil, err
	}
	return &Balance{
		Seconds: body.BalanceSeconds, Minutes: body.BalanceSeconds / 60, HeldSeconds: body.HeldSeconds,
		ExpiresAt: body.expiry(), PurchasesRemaining: body.PurchasesRemainingInWindow,
	}, nil
}

// Balance reads what the account can spend. It is net of holds: HeldSeconds says how much of the
// balance is already committed, and is not to be subtracted again.
func (c Client) Balance(ctx context.Context) (*Balance, error) {
	core, err := c.pipeline()
	if err != nil {
		return nil, err
	}
	balance, err := core.balance(ctx)
	if err != nil {
		return nil, (&annotator{core: core}).annotate(ctx, err)
	}
	return balance, nil
}

// Account reads the account: balance, limits, card on file and data region.
func (c Client) Account(ctx context.Context) (*Account, error) {
	core, err := c.pipeline()
	if err != nil {
		return nil, err
	}
	body, err := core.readAccount(ctx, "balance_seconds", "held_seconds", "limits")
	if err != nil {
		return nil, (&annotator{core: core}).annotate(ctx, err)
	}
	return &Account{
		BalanceSeconds: body.BalanceSeconds, HeldSeconds: body.HeldSeconds, CreditsExpireAt: body.expiry(),
		CardOnFile: body.CardOnFile, DataRegion: body.DataRegion, PurchasesRemainingInWindow: body.PurchasesRemainingInWindow,
		Limits: body.Limits.limits(),
	}, nil
}
