package hushscript

import (
	"fmt"
	"strings"
)

// vttCueText escapes what WebVTT reads as markup ("&" and "<") and as the end of a cue ("-->").
func vttCueText(text string) string {
	text = cueText(text)
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	return strings.ReplaceAll(text, "-->", "--&gt;")
}

// VTT renders the transcript as WebVTT subtitles, byte for byte what the Hushscript app exports.
func (t *Transcript) VTT(opts *FormatOptions) string {
	opts, utterances := t.cueOptions(opts)
	cues := cuesFor(utterances, opts)
	blocks := make([]string, len(cues))
	for i, c := range cues {
		blocks[i] = fmt.Sprintf("%d\n%s --> %s\n%s", i+1, srtTimestamp(c.startMs, "."), srtTimestamp(c.endMs, "."), vttCueText(withSpeaker(c)))
	}
	return "WEBVTT\n\n" + strings.Join(blocks, "\n\n") + "\n"
}
