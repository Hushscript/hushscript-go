package hushscript

import (
	"fmt"
	"regexp"
	"strings"
)

// srtTimestamp is "HH:MM:SS,mmm", floored to whole milliseconds; VTT swaps the comma for a dot.
func srtTimestamp(ms int, separator string) string {
	total := max(0, ms)
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", total/3_600_000, total%3_600_000/60_000, total%60_000/1000, separator, total%1000)
}

// cueText keeps a cue's line breaks but drops its blank lines: SRT and VTT end a cue at the first one.
func cueText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimFunc(line, isJSSpace) != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// cuesFor is the cues of utterances, re-cut into captions unless opts turns that off.
func cuesFor(utterances []Utterance, opts *FormatOptions) []cue {
	if opts != nil && !enabled(opts.Captions) {
		return utteranceCues(utterances)
	}
	return segmentCaptions(utterances)
}

// withSpeaker puts "{name}: " before the text only when the display name is not empty.
func withSpeaker(c cue) string {
	if c.speakerName == "" {
		return c.text
	}
	return c.speakerName + ": " + c.text
}

// An SRT reader takes a line holding "-->" for a timing line, so a run of dashes before ">" is cut to
// "->" (a plain "-->" swap would turn "--->" back into "-->").
var dashArrow = regexp.MustCompile(`-{2,}>`)

func (t *Transcript) cueOptions(opts *FormatOptions) (*FormatOptions, []Utterance) {
	if opts == nil {
		opts = &FormatOptions{}
	}
	return opts, t.UtterancesFor(opts.Language)
}

// SRT renders the transcript as SubRip subtitles, byte for byte what the Hushscript app exports.
func (t *Transcript) SRT(opts *FormatOptions) string {
	opts, utterances := t.cueOptions(opts)
	cues := cuesFor(utterances, opts)
	blocks := make([]string, len(cues))
	for i, c := range cues {
		text := dashArrow.ReplaceAllString(cueText(withSpeaker(c)), "->")
		blocks[i] = fmt.Sprintf("%d\n%s --> %s\n%s", i+1, srtTimestamp(c.startMs, ","), srtTimestamp(c.endMs, ","), text)
	}
	return strings.Join(blocks, "\n\n")
}
