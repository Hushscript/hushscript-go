// Signs an agent account up, waits for the checkout to be paid, claims the token and rotates it.
// The server shows each token once, so every one is written to the token file (mode 0600) the moment
// it arrives, before anything else can fail.
//
//	go run ./examples/agent-signup -terms 2026-10-01 -token-file agent-token
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	hushscript "github.com/Hushscript/hushscript-go"
)

func main() {
	terms := flag.String("terms", "", "the policy version you accept")
	tokenFile := flag.String("token-file", "", "where the tokens are kept")
	flag.Parse()
	if *terms == "" || *tokenFile == "" {
		fmt.Fprintln(os.Stderr, "usage: agent-signup -terms <policy version> -token-file <path>")
		os.Exit(2)
	}
	if err := run(*terms, *tokenFile); err != nil {
		var failure *hushscript.Error
		if errors.As(err, &failure) {
			fmt.Fprintf(os.Stderr, "%s: %s\n", failure.Code, failure.Hint+failure.Message)
			if failure.UnusableToken != "" {
				if keepErr := keep(*tokenFile, failure.UnusableToken); keepErr == nil {
					fmt.Fprintf(os.Stderr, "A token that came with the failed answer is in %s.\n", *tokenFile)
				}
			}
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		fmt.Fprintf(os.Stderr, "Any token that was issued is in %s.\n", *tokenFile)
		os.Exit(1)
	}
}

// keep writes the token to path with mode 0600: the file is created private, and made private again
// if it already existed, before any byte of the token is written.
func keep(path, token string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func run(terms, tokenFile string) error {
	ctx := context.Background()
	signup, err := hushscript.AgentSignup(ctx, hushscript.AgentSignupRequest{
		Email: "agent@example.com", Pack: "45min", AcceptTerms: terms,
		Agent: hushscript.AgentInfo{Name: "Example agent", Platform: "custom"},
	})
	if err != nil {
		return err
	}
	fmt.Printf("pay %s at %s\n", signup.Price.Formatted, signup.CheckoutURL)

	// The claim answers wrong_state until the checkout is paid. A lost answer is never retried.
	var account *hushscript.AgentAccount
	for account == nil {
		account, err = hushscript.AgentClaim(ctx, signup.SignupID, signup.ClaimSecret)
		var failure *hushscript.Error
		if errors.As(err, &failure) && failure.Kind == hushscript.KindConflict && failure.Code == "wrong_state" {
			select {
			case <-time.After(5 * time.Second):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err != nil {
			return err
		}
	}
	if saveErr := keep(tokenFile, account.Token); saveErr != nil { // before anything else can fail
		return saveErr
	}
	fmt.Printf("claimed %s; token saved to %s\n", account.UserID, tokenFile)

	client, err := hushscript.New(hushscript.WithToken(account.Token))
	if err != nil {
		return err
	}
	balance, err := client.Balance(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%d minutes\n", balance.Minutes)
	rotated, err := client.Agent.RotateToken(ctx) // revokes the claimed token; the client sends the new one
	if err != nil {
		return err
	}
	if err := keep(tokenFile, rotated.Token); err != nil {
		return err
	}
	fmt.Printf("rotated; scopes: %v; new token saved to %s\n", rotated.Scopes, tokenFile)
	return nil
}
