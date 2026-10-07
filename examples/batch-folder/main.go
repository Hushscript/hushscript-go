// Transcribes every recording in a folder, one after the other, and writes a text file for each.
//
//	go run ./examples/batch-folder recordings transcripts
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	hushscript "github.com/Hushscript/hushscript-go"
)

var audio = []string{".mp3", ".m4a", ".mp4", ".mov", ".wav", ".flac", ".ogg", ".opus"}

func main() {
	folder, out := "recordings", "transcripts"
	if len(os.Args) > 2 {
		folder, out = os.Args[1], os.Args[2]
	}
	if err := run(folder, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(folder, out string) error {
	client, err := hushscript.New()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o750); err != nil { //nolint:gosec // the output path is the operator's own argument
		return err
	}
	failed := false
	for _, entry := range entries {
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if entry.IsDir() || !slices.Contains(audio, ext) {
			continue
		}
		// Creates are paced for you (4 a minute), so a long folder just takes a while.
		transcript, err := client.Transcribe(context.Background(), filepath.Join(folder, entry.Name()), nil)
		if err != nil {
			failed = true
			var failure *hushscript.Error
			if errors.As(err, &failure) {
				fmt.Printf("failed %s: %s %s\n", entry.Name(), failure.Code, failure.Hint)
			} else {
				fmt.Printf("failed %s: %v\n", entry.Name(), err)
			}
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())) + ".txt"
		if err := os.WriteFile(filepath.Join(out, name), []byte(transcript.Text(nil)), 0o600); err != nil { //nolint:gosec // the output path is the operator's own argument
			return err
		}
		fmt.Println("done", entry.Name())
	}
	if failed {
		return errors.New("some recordings failed")
	}
	return nil
}
