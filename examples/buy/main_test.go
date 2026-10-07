package main

import (
	"testing"

	hushscript "github.com/Hushscript/hushscript-go"
)

func TestTheChargedWarningIsShownOnlyWhenThePurchaseMayHaveReachedTheServer(t *testing.T) {
	for name, tc := range map[string]struct {
		failure hushscript.Error
		want    bool
	}{
		"network drop":      {hushscript.Error{Kind: hushscript.KindNetwork, IdempotencyKey: "k"}, true},
		"lost answer":       {hushscript.Error{Kind: hushscript.KindOutcomeUnknown, IdempotencyKey: "k"}, true},
		"unusable 2xx":      {hushscript.Error{Kind: hushscript.KindAPI, Code: "bad_response", Status: 200, IdempotencyKey: "k"}, true},
		"server error":      {hushscript.Error{Kind: hushscript.KindAPI, Status: 503, IdempotencyKey: "k"}, true},
		"4021 refusal":      {hushscript.Error{Kind: hushscript.KindAPI, Status: 402, IdempotencyKey: "k"}, false},
		"429 refusal":       {hushscript.Error{Kind: hushscript.KindRateLimit, Status: 429, IdempotencyKey: "k"}, false},
		"4xx refusal":       {hushscript.Error{Kind: hushscript.KindAPI, Status: 403, IdempotencyKey: "k"}, false},
		"408 timeout":       {hushscript.Error{Kind: hushscript.KindAPI, Status: 408, IdempotencyKey: "k"}, true},
		"409 conflict":      {hushscript.Error{Kind: hushscript.KindConflict, Status: 409, IdempotencyKey: "k"}, true},
		"4023 refusal":      {hushscript.Error{Kind: hushscript.KindPermissionDenied, Status: 403, NumericCode: 4023, IdempotencyKey: "k"}, false},
		"no status":         {hushscript.Error{Kind: hushscript.KindAPI, IdempotencyKey: "k"}, true},
		"before the send":   {hushscript.Error{Kind: hushscript.KindNetwork}, false},
		"no card to charge": {hushscript.Error{Kind: hushscript.KindPaymentMethodRequired, Code: "no_default_card"}, false},
	} {
		if got := mayHaveCharged(&tc.failure); got != tc.want {
			t.Errorf("%s: mayHaveCharged = %v, want %v", name, got, tc.want)
		}
	}
}
