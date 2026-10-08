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

// refusals are the statuses of an answer that charged nothing: one the server decides from the request
// alone (400, a 403 for 4021 or 4023, 404, 422) or reaches after its completed-purchase replay (402).
// A 401 and a 429 are answered before that replay, so after the SDK's one retry they can follow a
// first attempt that did charge. Any other outcome after the request was sent, a 401, a 408, a 409, a
// 429, a 5xx, no status at all or an unusable answer, may have reached the card.
var refusals = map[int]bool{400: true, 402: true, 404: true, 422: true}

// mayHaveCharged reports whether a failed purchase may have reached the card: it was sent, and the
// answer was not one of the listed refusals.
func mayHaveCharged(failure *hushscript.Error) bool {
	refused := refusals[failure.Status] || failure.Status == 403 && (failure.NumericCode == 4021 || failure.NumericCode == 4023)
	return failure.IdempotencyKey != "" && !refused
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
