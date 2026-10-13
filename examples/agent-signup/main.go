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
	"path/filepath"
	"strings"
	"time"

	hushscript "github.com/Hushscript/hushscript-sdk-go"
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
			if token := failure.ReceivedToken(); isToken(token) {
				if keepErr := keep(*tokenFile, token); keepErr == nil {
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

// isToken reports whether the text has the shape of a token: the hsr prefix and printable ASCII
// only, which is what hushscript.New checks of a token it is given. It looks at the text alone, so
// an unrelated setting such as HUSHSCRIPT_BASE_URL cannot fail it.
func isToken(text string) bool {
	if !strings.HasPrefix(text, "hsr") {
		return false
	}
	for i := range len(text) {
		if text[i] < 0x21 || text[i] > 0x7e {
			return false
		}
	}
	return true
}

// keep writes the token to path with mode 0600. It goes to a private temporary file in the same
// directory first and replaces path by a rename, so the file never holds less than a whole token and
// an earlier token is never truncated before the new one is safely on disk.
func keep(path, token string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".token-*") // created with mode 0600
	if err != nil {
		return err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		_ = os.Remove(file.Name())
		return err
	}
	return nil
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
