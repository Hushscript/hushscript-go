package hushscript

import (
	"strings"
	"testing"
)

const nbsp = " "

// The table is the TypeScript SDK's money test: the two SDKs print the same price for the same answer.
func TestMoneyIsFormattedFromStripesMinorUnitDigitsNeverFromLocaleData(t *testing.T) {
	for _, tc := range []struct {
		amount   int
		currency string
		want     string
	}{
		{599, "USD", "$5.99"},
		{1, "USD", "$0.01"},
		{0, "USD", "$0.00"},
		{150_000, "USD", "$1,500.00"},
		{500, "JPY", "¥500"},
		{1500, "KWD", "KWD" + nbsp + "1.500"},
		{3499, "EUR", "€34.99"},
		{1250, "GBP", "£12.50"},
		// Stripe counts two minor digits where ICU counts none: the price is 100 times smaller than ICU's.
		{1_500_000, "IDR", "IDR" + nbsp + "15,000.00"},
		{150_000, "HUF", "HUF" + nbsp + "1,500.00"},
		{150_000, "ISK", "ISK" + nbsp + "1,500.00"},
		{150_000, "COP", "COP" + nbsp + "1,500.00"},
		// ... and two where ICU counts three.
		{1500, "LYD", "LYD" + nbsp + "15.00"},
		{1500, "BHD", "BHD" + nbsp + "1.500"},
		{250, "usd", "$2.50"},
		{-599, "USD", "-$5.99"},
		{-1500, "KWD", "-KWD" + nbsp + "1.500"},
		{1_234_567_890, "USD", "$12,345,678.90"},
	} {
		got := newMoney(tc.amount, tc.currency)
		if got.Formatted != tc.want || got.Amount != tc.amount || got.Currency != tc.currency {
			t.Errorf("newMoney(%d, %q) = %+v, want formatted %q", tc.amount, tc.currency, got, tc.want)
		}
	}
}

func TestEveryZeroAndThreeDigitCurrencyOfTheStripeTableHasItsDigits(t *testing.T) {
	for _, code := range []string{"BIF", "CLP", "DJF", "GNF", "JPY", "KMF", "KRW", "MGA", "PYG", "RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF"} {
		if got := newMoney(1234, code).Formatted; got != code+nbsp+"1,234" && got != "¥1,234" {
			t.Errorf("%s: %q has digits after the point or is not grouped", code, got)
		}
	}
	for _, code := range []string{"BHD", "JOD", "KWD", "OMR", "TND"} {
		if got := newMoney(1234, code).Formatted; got != code+nbsp+"1.234" {
			t.Errorf("%s: %q, want three digits", code, got)
		}
	}
}

// A code outside ISO 4217 is shown as the plain amount and the upper-cased code, with 2 digits, and
// never fails: the same strings the TypeScript SDK shows.
func TestAnUnknownCurrencyIsShownAsAPlainAmountAndCodeNeverFailing(t *testing.T) {
	for _, currency := range []string{"ZZZ", "ZZZZ", "", "us", "x1"} {
		got := newMoney(599, currency)
		want := Money{Amount: 599, Currency: currency, Formatted: "5.99 " + strings.ToUpper(currency)}
		if got != want {
			t.Errorf("newMoney(599, %q) = %+v, want %+v", currency, got, want)
		}
	}
	if got := newMoney(-150_000, "zzz").Formatted; got != "-1500.00 ZZZ" {
		t.Errorf("negative unknown: %q", got)
	}
}
