// Command smoke is the nightly production check: it reads the account of the token in
// HUSHSCRIPT_TOKEN through the SDK. It needs outbound network and runs only through `just smoke`.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	hushscript "github.com/Hushscript/hushscript-go"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "smoke:", err)
		os.Exit(1)
	}
}

func run() error {
	client, err := hushscript.New()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	response, err := client.API().GetAccountWithResponse(ctx)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		return fmt.Errorf("the account answer has no JSON body (status %d)", response.StatusCode())
	}
	fmt.Println("account readable; balance seconds:", response.JSON200.BalanceSeconds)
	return nil
}
