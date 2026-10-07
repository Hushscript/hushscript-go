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
	// knownCurrencies are the ISO 4217 codes the SDK prints with the code in front; any other string is
	// shown plainly, as "5.99 ZZZ", and never fails.
	knownCurrencies = setOf(
		"AED", "AFN", "ALL", "AMD", "ANG", "AOA", "ARS", "AUD", "AWG", "AZN", "BAM", "BBD", "BDT", "BGN", "BHD",
		"BIF", "BMD", "BND", "BOB", "BRL", "BSD", "BTN", "BWP", "BYN", "BZD", "CAD", "CDF", "CHF", "CLP", "CNY",
		"COP", "CRC", "CUP", "CVE", "CZK", "DJF", "DKK", "DOP", "DZD", "EGP", "ERN", "ETB", "EUR", "FJD", "FKP",
		"GBP", "GEL", "GHS", "GIP", "GMD", "GNF", "GTQ", "GYD", "HKD", "HNL", "HTG", "HUF", "IDR", "ILS", "INR",
		"IQD", "IRR", "ISK", "JMD", "JOD", "JPY", "KES", "KGS", "KHR", "KMF", "KPW", "KRW", "KWD", "KYD", "KZT",
		"LAK", "LBP", "LKR", "LRD", "LSL", "LYD", "MAD", "MDL", "MGA", "MKD", "MMK", "MNT", "MOP", "MRU", "MUR",
		"MVR", "MWK", "MXN", "MYR", "MZN", "NAD", "NGN", "NIO", "NOK", "NPR", "NZD", "OMR", "PAB", "PEN", "PGK",
		"PHP", "PKR", "PLN", "PYG", "QAR", "RON", "RSD", "RUB", "RWF", "SAR", "SBD", "SCR", "SDG", "SEK", "SGD",
		"SHP", "SLE", "SOS", "SRD", "SSP", "STN", "SVC", "SYP", "SZL", "THB", "TJS", "TMT", "TND", "TOP", "TRY",
		"TTD", "TWD", "TZS", "UAH", "UGX", "USD", "UYU", "UZS", "VES", "VND", "VUV", "WST", "XAF", "XCD", "XOF",
		"XPF", "YER", "ZAR", "ZMW", "ZWG",
	)
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
	whole := strconv.FormatUint(magnitude/scale, 10)
	number := groupThousands(whole)
	if !knownCurrencies[code] {
		number = whole // the plain fallback is not grouped
	}
	if digits > 0 {
		fraction := strconv.FormatUint(magnitude%scale, 10)
		number += "." + strings.Repeat("0", digits-len(fraction)) + fraction
	}
	sign := ""
	if amount < 0 {
		sign = "-"
	}
	if !knownCurrencies[code] {
		return Money{Amount: amount, Currency: currency, Formatted: sign + number + " " + code}
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
