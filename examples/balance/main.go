// Prints the balance, the latest transcripts and the jobs still running.
//
//	go run ./examples/balance
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	hushscript "github.com/Hushscript/hushscript-sdk-go"
)

func main() {
	if err := run(); err != nil {
		var failure *hushscript.Error
		if errors.As(err, &failure) {
			fmt.Fprintf(os.Stderr, "%s: %s\n", failure.Code, failure.Message)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	client, err := hushscript.New()
	if err != nil {
		return err
	}
	balance, err := client.Balance(ctx)
	if err != nil {
		return err
	}
	// Held minutes are already taken out of Minutes; do not subtract them again.
	fmt.Printf("%d minutes to spend, %d s held by running jobs\n", balance.Minutes, balance.HeldSeconds)

	shown := 0
	for transcript, err := range client.Transcripts.List(ctx, &hushscript.ListOptions{PageSize: 5}) {
		if err != nil {
			return err
		}
		fmt.Println(transcript.ID, transcript.Title)
		shown++
		if shown == 5 {
			break
		}
	}
	// Transcripts that are still processing are jobs, not transcripts yet.
	for job, err := range client.Jobs.List(ctx, &hushscript.JobListOptions{Active: true}) {
		if err != nil {
			return err
		}
		fmt.Println(job.ID, job.State)
	}
	return nil
}
