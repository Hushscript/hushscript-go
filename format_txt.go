package hushscript

import (
	"fmt"
	"strings"
)

type textBlock struct {
	speaker     string
	speakerName string
	startMs     int
	text        string
}

func textBlocks(utterances []Utterance, merge bool) []textBlock {
	var blocks []textBlock
	for _, u := range utterances {
		if last := len(blocks) - 1; merge && last >= 0 && blocks[last].speaker == u.Speaker {
			blocks[last].text += " " + u.Text
			continue
		}
		blocks = append(blocks, textBlock{speaker: u.Speaker, speakerName: u.SpeakerName, startMs: u.StartMs, text: u.Text})
	}
	return blocks
}

// clock is "M:SS" or "H:MM:SS" from milliseconds, floored to whole seconds.
func clock(ms int) string {
	seconds := max(0, ms) / 1000
	h, m, s := seconds/3600, seconds%3600/60, seconds%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// Text renders the transcript as plain text: "Name: text" blocks joined by a blank line. "\r" and
// whitespace-only lines are kept as they come, as in the app.
func (t *Transcript) Text(opts *TextOptions) string {
	if opts == nil {
		opts = &TextOptions{}
	}
	blocks := textBlocks(t.UtterancesFor(opts.Language), enabled(opts.Merge))
	out := make([]string, len(blocks))
	for i, b := range blocks {
		who := b.speakerName
		if !enabled(opts.Speakers) {
			who = ""
		}
		prefix := ""
		if opts.Timestamps {
			prefix = "[" + clock(b.startMs) + "] "
		}
		if who == "" {
			out[i] = prefix + b.text
		} else {
			out[i] = prefix + who + ": " + b.text
		}
	}
	return strings.Join(out, "\n\n")
}
