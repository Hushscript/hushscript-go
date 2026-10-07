// Spends money: it needs a pack id and -yes before it charges the saved card.
//
//	go run ./examples/buy -pack 300min          shows the packs and the notice, buys nothing
//	go run ./examples/buy -pack 300min -yes     buys the pack
//	go run ./examples/buy -pack 300min -yes -key KEY   asks again with the key of an earlier attempt
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	hushscript "github.com/Hushscript/hushscript-go"
)

const notice = "Minutes are added to your account instantly after payment. All purchases are final and non-refundable."

func main() {
	pack := flag.String("pack", "", "the pack id to buy, one of the packs listed")
	yes := flag.Bool("yes", false, "charge the saved card")
	key := flag.String("key", "", "the idempotency key an earlier attempt printed")
	flag.Parse()
	keySet := false
	flag.Visit(func(f *flag.Flag) { keySet = keySet || f.Name == "key" })
	if keySet && *key == "" {
		fmt.Fprintln(os.Stderr, "-key needs a value: the key an earlier attempt printed")
		os.Exit(2)
	}
	if err := run(*pack, *yes, *key); err != nil {
		report(err)
		os.Exit(1)
	}
}

func run(pack string, yes bool, key string) error {
	ctx := context.Background()
	client, err := hushscript.New()
	if err != nil {
		return err
	}
	packs, err := client.Billing.Packs(ctx)
	if err != nil {
		return err
	}
	var chosen *hushscript.Pack
	for i := range packs {
		fmt.Printf("%s: %d minutes for %s\n", packs[i].ID, packs[i].Minutes, packs[i].Price.Formatted)
		if packs[i].ID == pack {
			chosen = &packs[i]
		}
	}
	if chosen == nil {
		fmt.Println("Name one of the packs above with -pack to buy it.")
		os.Exit(2)
	}
	// Show the buyer this notice before the call; AcknowledgeNoRefund records that it was shown.
	fmt.Println(notice)
	if !yes {
		fmt.Printf("Nothing was bought. Run again with -yes to charge your saved card %s for %s.\n", chosen.Price.Formatted, chosen.ID)
		os.Exit(1)
	}
	purchase, err := client.Buy(ctx, chosen.ID, &hushscript.BuyOptions{AcknowledgeNoRefund: true, IdempotencyKey: key})
	if err != nil {
		return err
	}
	fmt.Printf("bought %s; now %d minutes\n", chosen.ID, purchase.Minutes)
	return nil
}

// mayHaveCharged reports whether a failed purchase may have reached the card: the answer was lost,
// unusable or a server error. A refusal (4021, 4023, 429 and every other 4xx) charged nothing.
func mayHaveCharged(failure *hushscript.Error) bool {
	if failure.IdempotencyKey == "" {
		return false
	}
	lost := failure.Kind == hushscript.KindNetwork || failure.Kind == hushscript.KindAbort || failure.Kind == hushscript.KindOutcomeUnknown
	return lost || failure.Code == "bad_response" || failure.Status >= 500
}

func report(err error) {
	var failure *hushscript.Error
	if !errors.As(err, &failure) {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	text := failure.Hint
	if text == "" {
		text = failure.Message
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", failure.Code, text)
	if mayHaveCharged(failure) {
		fmt.Fprintf(os.Stderr, "The card may have been charged. Run again with the same pack and -key %s: it cannot charge twice.\n", failure.IdempotencyKey)
	}
}
