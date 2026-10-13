// Transcribes one recording and writes it as subtitles.
//
//	go run ./examples/export-srt meeting.mp3 meeting.srt
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	hushscript "github.com/Hushscript/hushscript-sdk-go"
)

func main() {
	source, out := "meeting.mp3", "meeting.srt"
	if len(os.Args) > 2 {
		source, out = os.Args[1], os.Args[2]
	}
	if err := run(source, out); err != nil {
		var failure *hushscript.Error
		if errors.As(err, &failure) {
			fmt.Fprintf(os.Stderr, "%s: %s\n", failure.Code, failure.Message)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func run(source, out string) error {
	client, err := hushscript.New()
	if err != nil {
		return err
	}
	transcript, err := client.Transcribe(context.Background(), source, nil)
	if err != nil {
		return err
	}
	// SRT and VTT wrap lines into caption cues; &hushscript.FormatOptions{Captions: hushscript.Bool(false)}
	// gives one cue per utterance.
	if err := os.WriteFile(out, []byte(transcript.SRT(nil)), 0o600); err != nil { //nolint:gosec // the output path is the operator's own argument
		return err
	}
	fmt.Println("wrote", out)
	return nil
}
