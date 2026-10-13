// Transcribes one recording and prints the transcript.
//
//	go run ./examples/transcribe meeting.mp3
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	hushscript "github.com/Hushscript/hushscript-sdk-go"
)

func main() {
	path := "meeting.mp3"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	if err := run(path); err != nil {
		report(err)
		os.Exit(1)
	}
}

func run(path string) error {
	client, err := hushscript.New() // reads HUSHSCRIPT_TOKEN
	if err != nil {
		return err
	}
	transcript, err := client.Transcribe(context.Background(), path, &hushscript.TranscribeOptions{Title: "Weekly sync"})
	if err != nil {
		return err
	}
	fmt.Printf("%s: %d utterances\n", transcript.Title, len(transcript.Utterances))
	fmt.Println(transcript.Text(nil))
	return nil
}

func report(err error) {
	var failure *hushscript.Error
	if !errors.As(err, &failure) {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", failure.Code, firstNonEmpty(failure.Hint, failure.Message))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
