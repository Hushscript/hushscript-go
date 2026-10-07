// Package goldens lists the golden files of the conformance suite (fixtures/exports) and says how each
// one is rendered. The golden test and the conformance runner's format op share it, so the two cannot
// drift apart.
package goldens

import (
	"encoding/json"

	hushscript "github.com/Hushscript/hushscript-go"
)

// Transcripts are the transcript fixtures (fixtures/transcripts, without .json).
var Transcripts = []string{"escapes", "long-utterances", "multi-renamed", "no-labels", "one-speaker", "over-hour", "translated"}

// Only this transcript has translation and cleaned goldens, named <name>.<variant>.<language>.<ext>.
const languageTranscript = "translated"

var languages = []string{"es", "cleaned"}

func no() *bool {
	v := false
	return &v
}

func yes() *bool {
	v := true
	return &v
}

type variant struct {
	name   string
	ext    string
	render func(t *hushscript.Transcript, language string) string
}

var variants = []variant{
	{"srt-captions", "srt", func(t *hushscript.Transcript, l string) string { return t.SRT(&hushscript.FormatOptions{Language: l}) }},
	{"srt-plain", "srt", func(t *hushscript.Transcript, l string) string {
		return t.SRT(&hushscript.FormatOptions{Language: l, Captions: no()})
	}},
	{"vtt-captions", "vtt", func(t *hushscript.Transcript, l string) string { return t.VTT(&hushscript.FormatOptions{Language: l}) }},
	{"vtt-plain", "vtt", func(t *hushscript.Transcript, l string) string {
		return t.VTT(&hushscript.FormatOptions{Language: l, Captions: no()})
	}},
	{"txt-default", "txt", func(t *hushscript.Transcript, l string) string { return t.Text(&hushscript.TextOptions{Language: l}) }},
	{"txt-nospeakers", "txt", func(t *hushscript.Transcript, l string) string {
		return t.Text(&hushscript.TextOptions{Language: l, Speakers: no()})
	}},
	{"txt-timestamps", "txt", func(t *hushscript.Transcript, l string) string {
		return t.Text(&hushscript.TextOptions{Language: l, Timestamps: true, Speakers: yes()})
	}},
	{"txt-nomerge", "txt", func(t *hushscript.Transcript, l string) string {
		return t.Text(&hushscript.TextOptions{Language: l, Merge: no()})
	}},
}

// Golden is one golden file and how to produce it.
type Golden struct {
	// File is the name under fixtures/exports.
	File string
	// Transcript is the fixture under fixtures/transcripts, without .json.
	Transcript string
	// Language is "" for the original.
	Language string
	render   func(t *hushscript.Transcript, language string) string
}

// All lists every golden from the naming scheme: <transcript>.<variant>[.<language>].<ext>.
func All() []Golden {
	var out []Golden
	for _, transcript := range Transcripts {
		names := []string{""}
		if transcript == languageTranscript {
			names = append(names, languages...)
		}
		for _, v := range variants {
			for _, language := range names {
				middle := ""
				if language != "" {
					middle = "." + language
				}
				out = append(out, Golden{File: transcript + "." + v.name + middle + "." + v.ext, Transcript: transcript, Language: language, render: v.render})
			}
		}
	}
	return out
}

// Render renders the golden from the body of its transcript fixture.
func Render(g Golden, body []byte) (string, error) {
	var transcript hushscript.Transcript
	if err := json.Unmarshal(body, &transcript); err != nil {
		return "", err
	}
	return g.render(&transcript, g.Language), nil
}
