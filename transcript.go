package hushscript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// reservedCleaned is the translation key that holds the AI-cleaned body.
const reservedCleaned = "cleaned"

// Utterance is one speaker turn of a transcript.
type Utterance struct {
	// Speaker is the provider's raw label; empty for a transcript without speaker labels.
	Speaker string
	// SpeakerName is the saved name, else "Speaker {label}", else "" for an empty label.
	SpeakerName string
	StartMs     int
	EndMs       int
	Text        string
}

// Transcript is a finished transcription with its formatters (spec 6.2). It is read-only after it was
// built from the API's body, by Transcribe, Transcripts.Get or json.Unmarshal.
type Transcript struct {
	ID    string
	Title string
	// Language is the detected language; "" when there is none.
	Language        string
	DurationSeconds int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// AutoDeleteAt is when the transcript is scheduled to be purged; nil keeps it indefinitely.
	AutoDeleteAt *time.Time
	Tags         []string
	Edited       bool
	// Speakers maps a label to its display name, for the speakers that were renamed.
	Speakers map[string]string
	// Utterances are in the original language.
	Utterances []Utterance
	// Languages are the translation locales, without the reserved "cleaned", in the API's order.
	Languages []string
	// Cleaned is the AI-cleaned body; nil when there is none.
	Cleaned []Utterance
	// Raw is the API's body as it came, unknown fields included; it is what MarshalJSON writes.
	Raw json.RawMessage

	translations map[string][]Utterance
}

type utteranceBody struct {
	Speaker string          `json:"speaker"`
	StartMs timestampNumber `json:"start_ms"`
	EndMs   timestampNumber `json:"end_ms"`
	Text    string          `json:"text"`
}

type timestampNumber json.Number

func (n *timestampNumber) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return errors.New("hushscript: an utterance timestamp must be a number")
	}
	return json.Unmarshal(data, (*json.Number)(n))
}

type transcriptBody struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	LanguageDetected *string           `json:"language_detected"`
	DurationSeconds  int               `json:"duration_seconds"`
	SpeakerNames     map[string]string `json:"speaker_names"`
	Tags             []string          `json:"tags"`
	CreatedAt        *int64            `json:"created_at"`
	UpdatedAt        *int64            `json:"updated_at"`
	AutoDeleteAt     *int64            `json:"auto_delete_at"`
	Edited           bool              `json:"edited"`
	Body             struct {
		Original struct {
			Utterances []utteranceBody `json:"utterances"`
		} `json:"original"`
		Translations json.RawMessage `json:"translations"`
	} `json:"body"`
}

// transcriptRequired is what a transcript body must hold for the formatters to mean anything.
var transcriptRequired = []string{"id", "created_at", "body.original.utterances"}

// UnmarshalJSON builds a Transcript from the body of GET /v1/transcripts/{id}.
func (t *Transcript) UnmarshalJSON(data []byte) error {
	built, err := parseTranscript(data)
	if err != nil {
		return err
	}
	*t = *built
	return nil
}

// MarshalJSON writes the API body the Transcript was built from; a Transcript that was not built from
// one writes the fields it holds.
func (t Transcript) MarshalJSON() ([]byte, error) {
	if len(t.Raw) > 0 {
		return t.Raw, nil
	}
	return json.Marshal(struct {
		ID              string `json:"id"`
		Title           string `json:"title"`
		DurationSeconds int    `json:"duration_seconds"`
	}{t.ID, t.Title, t.DurationSeconds})
}

func unixTime(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }

func parseTranscript(data []byte) (*Transcript, error) {
	if !hasFields(data, transcriptRequired) {
		return nil, errors.New("hushscript: a transcript needs an id, created_at and body.original.utterances")
	}
	var body transcriptBody
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("hushscript: reading a transcript: %w", err)
	}
	names := body.SpeakerNames
	if names == nil {
		names = map[string]string{}
	}
	t := &Transcript{
		ID: body.ID, Title: body.Title, DurationSeconds: body.DurationSeconds, Tags: body.Tags, Edited: body.Edited,
		Speakers: names, Raw: json.RawMessage(bytes.Clone(data)), translations: map[string][]Utterance{},
	}
	if t.Tags == nil {
		t.Tags = []string{}
	}
	if body.LanguageDetected != nil {
		t.Language = *body.LanguageDetected
	}
	if body.CreatedAt != nil {
		t.CreatedAt = unixTime(*body.CreatedAt)
	}
	if body.UpdatedAt != nil {
		t.UpdatedAt = unixTime(*body.UpdatedAt)
	}
	if body.AutoDeleteAt != nil {
		at := unixTime(*body.AutoDeleteAt)
		t.AutoDeleteAt = &at
	}
	var err error
	t.Utterances, err = utterancesOf(body.Body.Original.Utterances, names)
	if err != nil {
		return nil, err
	}

	var translations map[string]struct {
		Utterances []utteranceBody `json:"utterances"`
	}
	if len(body.Body.Translations) > 0 && string(body.Body.Translations) != "null" {
		if err := json.Unmarshal(body.Body.Translations, &translations); err != nil {
			return nil, fmt.Errorf("hushscript: reading the translations of a transcript: %w", err)
		}
		order, err := objectKeys(body.Body.Translations)
		if err != nil {
			return nil, fmt.Errorf("hushscript: reading the translations of a transcript: %w", err)
		}
		for _, locale := range order {
			t.translations[locale], err = utterancesOf(translations[locale].Utterances, names)
			if err != nil {
				return nil, err
			}
			if locale != reservedCleaned {
				t.Languages = append(t.Languages, locale)
			}
		}
		t.Cleaned = t.translations[reservedCleaned]
	}
	return t, nil
}

// objectKeys lists the keys of a JSON object in document order, first occurrence only.
func objectKeys(raw json.RawMessage) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var keys []string
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
		var skipped json.RawMessage
		if err := decoder.Decode(&skipped); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func utterancesOf(raw []utteranceBody, names map[string]string) ([]Utterance, error) {
	out := make([]Utterance, len(raw))
	for i, u := range raw {
		start, startOK := timestampMs(json.Number(u.StartMs))
		end, endOK := timestampMs(json.Number(u.EndMs))
		if !startOK || !endOK {
			return nil, errors.New("hushscript: an utterance timestamp is out of range")
		}
		out[i] = Utterance{
			Speaker: u.Speaker, SpeakerName: displayName(u.Speaker, names),
			StartMs: start, EndMs: end, Text: u.Text,
		}
	}
	return out, nil
}

func timestampMs(value json.Number) (int, bool) {
	if value == "" {
		return 0, true
	}
	if whole, err := strconv.ParseInt(string(value), 10, strconv.IntSize); err == nil {
		return int(whole), true
	} else if errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	fraction, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		return 0, false
	}
	whole := math.Floor(fraction)
	bound := math.Ldexp(1, strconv.IntSize-1)
	if math.IsNaN(whole) || math.IsInf(whole, 0) || whole < -bound || whole >= bound {
		return 0, false
	}
	return int(whole), true
}

// displayName is the saved name if it is not blank, else "" for a blank label, else "Speaker {label}".
func displayName(label string, names map[string]string) string {
	if saved := strings.TrimFunc(names[label], isJSSpace); saved != "" {
		return saved
	}
	if strings.TrimFunc(label, isJSSpace) == "" {
		return ""
	}
	return "Speaker " + label
}

// UtterancesFor returns the translation (or "cleaned" body) for language, and the original when
// language is empty or the transcript has no such translation.
func (t *Transcript) UtterancesFor(language string) []Utterance {
	if found, ok := t.translations[language]; ok && language != "" {
		return found
	}
	return t.Utterances
}

// FormatOptions are the options of SRT and VTT. The zero value gives readable captions of the original
// language.
type FormatOptions struct {
	// Language is a translation locale or "cleaned"; "" and an unknown locale give the original.
	Language string
	// Captions re-cuts the text into cues of at most two lines of 42 characters; nil means true. False
	// gives one cue per utterance.
	Captions *bool
}

// TextOptions are the options of Text. The zero value gives the original language with speaker names,
// consecutive turns of one speaker merged, and no timestamps.
type TextOptions struct {
	Language string
	// Speakers prefixes each block with its speaker; nil means true.
	Speakers *bool
	// Timestamps prefixes each block with [M:SS] or [H:MM:SS].
	Timestamps bool
	// Merge joins consecutive utterances of the same speaker label; nil means true.
	Merge *bool
}

func enabled(flag *bool) bool { return flag == nil || *flag }
