package hushscript

import (
	"math"
	"strings"
)

// Caption limits (spec 6.2). Line length is counted in UTF-16 code units, as the app does.
const (
	maxCaptionChars = 42
	maxCaptionLines = 2
	maxCueMs        = 7000
)

// cue is one subtitle cue; speakerName is empty when the cue carries no speaker prefix.
type cue struct {
	startMs     int
	endMs       int
	speakerName string
	text        string
}

// isJSSpace is JavaScript's whitespace (the set \s and String.prototype.trim use), which is not
// unicode.IsSpace: it lacks U+0085 and has U+FEFF.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// utf16Len is the length JavaScript reports for s.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

// roundHalfUp is Math.round for non-negative values.
func roundHalfUp(x float64) int {
	floor := math.Floor(x)
	if x-floor >= 0.5 {
		floor++
	}
	return int(floor)
}

// wrapIntoCues word-wraps text into cues of at most maxLines lines of maxChars; an overlong word keeps
// a line to itself. Text without a word gives one empty cue.
func wrapIntoCues(text string, maxChars, maxLines int) []string {
	words := strings.FieldsFunc(text, isJSSpace)
	if len(words) == 0 {
		return []string{""}
	}
	var cues, lines []string
	line, lineLen := "", 0
	flushLine := func() {
		if line != "" {
			lines = append(lines, line)
		}
		line, lineLen = "", 0
	}
	flushCue := func() {
		flushLine()
		if len(lines) > 0 {
			cues = append(cues, strings.Join(lines, "\n"))
		}
		lines = nil
	}
	for _, word := range words {
		wordLen := utf16Len(word)
		candidate, candidateLen := word, wordLen
		if line != "" {
			candidate, candidateLen = line+" "+word, lineLen+1+wordLen
		}
		if candidateLen <= maxChars {
			line, lineLen = candidate, candidateLen
			continue
		}
		flushLine()
		if len(lines) >= maxLines {
			flushCue()
		}
		line, lineLen = word, wordLen
	}
	flushCue()
	if len(cues) == 0 {
		return []string{""}
	}
	return cues
}

// utteranceCues is one cue per utterance (captions off).
func utteranceCues(utterances []Utterance) []cue {
	cues := make([]cue, len(utterances))
	for i, u := range utterances {
		cues[i] = cue{startMs: u.StartMs, endMs: u.EndMs, speakerName: u.SpeakerName, text: u.Text}
	}
	return cues
}

// segmentCaptions re-cuts utterances into readable cues: lines of at most 42 characters, two lines to
// a cue, the utterance span split by character share, and every cue, the last of an utterance
// included, capped at 7000 ms. Cue positions come from the cumulative character share, so the cap
// never shifts a later cue.
func segmentCaptions(utterances []Utterance) []cue {
	var out []cue
	for _, u := range utterances {
		chunks := wrapIntoCues(u.Text, maxCaptionChars, maxCaptionLines)
		span := max(0, u.EndMs-u.StartMs)
		total := 0
		for _, chunk := range chunks {
			total += utf16Len(chunk)
		}
		if total == 0 {
			total = 1
		}
		share := func(chars int) int { return roundHalfUp(float64(span*chars) / float64(total)) }
		acc := 0
		for i, text := range chunks {
			startMs := u.StartMs + share(acc)
			acc += max(1, utf16Len(text))
			endMs := u.EndMs
			if i != len(chunks)-1 {
				endMs = u.StartMs + share(acc)
			}
			if endMs-startMs > maxCueMs {
				endMs = startMs + maxCueMs
			}
			endMs = max(endMs, startMs)
			out = append(out, cue{startMs: startMs, endMs: endMs, speakerName: u.SpeakerName, text: text})
		}
	}
	return out
}
