package hushscript

import (
	"strconv"
	"strings"
)

// Money is a price as the API states it, with a display string in the en locale.
type Money struct {
	// Amount is in the currency's minor units, from the API.
	Amount int
	// Currency is the ISO 4217 code as the API sends it.
	Currency string
	// Formatted is Amount divided by Stripe's minor-unit digits for the currency.
	Formatted string
}

// Stripe's minor-unit digits, which the API's amounts use (spec 6.5). They come from this table and
// never from locale data: ICU disagrees for IDR, HUF, ISK and others.
var (
	zeroDigitCurrencies  = setOf("BIF", "CLP", "DJF", "GNF", "JPY", "KMF", "KRW", "MGA", "PYG", "RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF")
	threeDigitCurrencies = setOf("BHD", "JOD", "KWD", "OMR", "TND")
	// currencySymbols are the symbols the en locale prints; any other currency shows its code.
	currencySymbols = map[string]string{"USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥"}
)

func setOf(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

func minorUnitDigits(code string) int {
	switch {
	case zeroDigitCurrencies[code]:
		return 0
	case threeDigitCurrencies[code]:
		return 3
	default:
		return 2
	}
}

func newMoney(amount int, currency string) Money {
	code := strings.ToUpper(currency)
	digits := minorUnitDigits(code)
	magnitude := uint64(amount) //nolint:gosec // the sign is handled on the next line
	if amount < 0 {
		magnitude = -magnitude
	}
	scale := uint64(1)
	for range digits {
		scale *= 10
	}
	number := groupThousands(strconv.FormatUint(magnitude/scale, 10))
	if digits > 0 {
		fraction := strconv.FormatUint(magnitude%scale, 10)
		number += "." + strings.Repeat("0", digits-len(fraction)) + fraction
	}
	sign := ""
	if amount < 0 {
		sign = "-"
	}
	if symbol, ok := currencySymbols[code]; ok {
		return Money{Amount: amount, Currency: currency, Formatted: sign + symbol + number}
	}
	return Money{Amount: amount, Currency: currency, Formatted: sign + code + " " + number}
}

func groupThousands(digits string) string {
	var b strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}
