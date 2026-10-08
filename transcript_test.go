package hushscript

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func flag(v bool) *bool { return &v }

// transcriptWith builds a transcript from utterances (a JSON array) and optional extra top-level keys.
func transcriptWith(t *testing.T, utterances, extra string) *Transcript {
	t.Helper()
	body := fmt.Sprintf(`{"id":"tr_x","title":"T","language_detected":"en","duration_seconds":60,"speaker_names":{},"tags":[],`+
		`"created_at":1760000000,"updated_at":1760000100,"auto_delete_at":null,"edited":false,%s"body":{"version":1,"original":{"utterances":%s},"translations":{}}}`, extra, utterances)
	var tr Transcript
	if err := json.Unmarshal([]byte(body), &tr); err != nil {
		t.Fatalf("building the transcript: %v", err)
	}
	return &tr
}

func one(speaker string, start, end int, text string) string {
	encoded, _ := json.Marshal(text)
	return fmt.Sprintf(`{"speaker":%q,"start_ms":%d,"end_ms":%d,"text":%s}`, speaker, start, end, encoded)
}

func list(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

func TestATranscriptKeepsEveryFieldOfTheBodyAndTheBodyItself(t *testing.T) {
	raw := `{"id":"tr_1","title":"Standup","language_detected":null,"duration_seconds":61,"speaker_names":{"A":"  Ann  ","B":"   "},"tags":null,` +
		`"created_at":1760000000,"updated_at":1760000100,"auto_delete_at":1770000000,"edited":true,"x_unknown":{"k":[1,2]},` +
		`"body":{"version":1,"original":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":900,"text":"hi"},{"speaker":"B","start_ms":900,"end_ms":1800,"text":"yo"},{"speaker":"","start_ms":1800,"end_ms":2000,"text":"x"}]},` +
		`"translations":{"fr":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":900,"text":"salut"}]},"cleaned":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":900,"text":"Hi."}]},"de":{"utterances":[]}}}}`
	var tr Transcript
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.ID != "tr_1" || tr.Title != "Standup" || tr.Language != "" || tr.DurationSeconds != 61 || !tr.Edited {
		t.Errorf("scalars: %+v", tr)
	}
	if tr.CreatedAt.Unix() != 1760000000 || tr.CreatedAt.Location().String() != "UTC" || tr.UpdatedAt.Unix() != 1760000100 {
		t.Errorf("times: %v %v", tr.CreatedAt, tr.UpdatedAt)
	}
	if tr.AutoDeleteAt == nil || tr.AutoDeleteAt.Unix() != 1770000000 {
		t.Errorf("AutoDeleteAt = %v", tr.AutoDeleteAt)
	}
	if tr.Tags == nil || len(tr.Tags) != 0 {
		t.Errorf("Tags = %#v, want an empty slice for null", tr.Tags)
	}
	want := []Utterance{
		{Speaker: "A", SpeakerName: "Ann", StartMs: 0, EndMs: 900, Text: "hi"},
		{Speaker: "B", SpeakerName: "Speaker B", StartMs: 900, EndMs: 1800, Text: "yo"},
		{Speaker: "", SpeakerName: "", StartMs: 1800, EndMs: 2000, Text: "x"},
	}
	withoutRaw := make([]Utterance, len(tr.Utterances))
	for i, u := range tr.Utterances {
		if len(u.Raw) == 0 {
			t.Errorf("utterance %d has no Raw", i)
		}
		u.Raw = nil
		withoutRaw[i] = u
	}
	if fmt.Sprint(withoutRaw) != fmt.Sprint(want) {
		t.Errorf("utterances = %v, want %v", withoutRaw, want)
	}
	if got := strings.Join(tr.Languages, ","); got != "fr,de" {
		t.Errorf("Languages = %q, want the API's order without cleaned", got)
	}
	if len(tr.Cleaned) != 1 || tr.Cleaned[0].Text != "Hi." || tr.Cleaned[0].SpeakerName != "Ann" {
		t.Errorf("Cleaned = %v", tr.Cleaned)
	}
	if got := tr.UtterancesFor("fr")[0].Text; got != "salut" {
		t.Errorf("fr = %q", got)
	}
	if got := tr.UtterancesFor("cleaned")[0].Text; got != "Hi." {
		t.Errorf("cleaned = %q", got)
	}
	for _, language := range []string{"", "original", "xx"} {
		if got := tr.UtterancesFor(language); len(got) != 3 {
			t.Errorf("UtterancesFor(%q) has %d utterances, want the original's 3", language, len(got))
		}
	}
	if got := tr.UtterancesFor("de"); len(got) != 0 {
		t.Errorf("an empty translation must not fall back to the original: %v", got)
	}
	again, err := json.Marshal(tr)
	if err != nil || string(again) != raw {
		t.Errorf("Marshal = %s, %v; want the body it was built from", again, err)
	}
	if !strings.Contains(string(tr.Raw), `"x_unknown":{"k":[1,2]}`) {
		t.Errorf("Raw lost an unknown field: %s", tr.Raw)
	}
}

func TestATranscriptWithoutWhatTheFormattersNeedIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"not an object":      `[]`,
		"null":               `null`,
		"no id":              `{"title":"T","body":{"original":{"utterances":[]}}}`,
		"no body":            `{"id":"x"}`,
		"null utterances":    `{"id":"x","created_at":1,"body":{"original":{"utterances":null}}}`,
		"mistyped timestamp": `{"id":"x","created_at":1,"body":{"original":{"utterances":[{"speaker":"A","start_ms":"0","end_ms":1,"text":"a"}]}}}`,
		"mistyped locale":    `{"id":"x","created_at":1,"body":{"original":{"utterances":[]},"translations":{"fr":7}}}`,
		"mistyped list":      `{"id":"x","created_at":1,"body":{"original":{"utterances":[]},"translations":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScriptedServer(t, step{status: 200, body: body})
			c := newTestClient(t, srv.URL, newFakeClock())
			_, err := c.Transcripts.Get(context.Background(), "x")
			assertCoreError(t, err, KindAPI, "bad_response", 200)
			if srv.count() != 1 {
				t.Errorf("requests = %d, want 1", srv.count())
			}
			var tr Transcript
			if err := json.Unmarshal([]byte(body), &tr); err == nil {
				t.Errorf("accepted %s as %+v", body, tr)
			}
		})
	}
}

func TestAZeroTranscriptMarshalsWithoutPanicking(t *testing.T) {
	out, err := json.Marshal(Transcript{ID: "a", Title: "b", DurationSeconds: 2})
	if err != nil || string(out) != `{"id":"a","title":"b","duration_seconds":2}` {
		t.Errorf("Marshal = %s, %v", out, err)
	}
}

func TestSpeakerNamesTrimLikeJavaScript(t *testing.T) {
	names := map[string]string{"A": "\uFEFF Ann \u00a0", "B": "\u0085", "C": "", "constructor": "Ctor"}
	for label, want := range map[string]string{
		"A": "Ann", "B": "\u0085" /* U+0085 is not JavaScript whitespace */, "C": "Speaker C", "D": "Speaker D",
		"": "", " ": "", "\uFEFF": "", "\u0085x": "Speaker \u0085x", "constructor": "Ctor", "toString": "Speaker toString",
	} {
		if got := displayName(label, names); got != want {
			t.Errorf("displayName(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestRoundHalfUpIsMathRound(t *testing.T) {
	for in, want := range map[float64]int{0: 0, 0.4999: 0, 0.49999999999999994: 0, 0.5: 1, 1.5: 2, 2.5: 3, 500.5: 501, 6099.99: 6100} {
		if got := roundHalfUp(in); got != want {
			t.Errorf("roundHalfUp(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestLineLengthIsCountedInUTF16CodeUnits(t *testing.T) {
	if got := utf16Len("a😀é\U0001F468\u200d\U0001F469"); got != 1+2+1+2+1+2 {
		t.Errorf("utf16Len = %d", got)
	}
	words := strings.TrimSpace(strings.Repeat("😀 ", 15)) // 15 emoji: 29 runes, 44 UTF-16 units
	tr := transcriptWith(t, list(one("", 0, 3000, words)), "")
	got := tr.SRT(nil)
	want := "1\n00:00:00,000 --> 00:00:03,000\n" + strings.TrimSpace(strings.Repeat("😀 ", 14)) + "\n😀"
	if got != want {
		t.Errorf("SRT = %q\nwant %q", got, want)
	}
	fits := transcriptWith(t, list(one("", 0, 3000, strings.TrimSpace(strings.Repeat("😀 ", 14)))), "")
	if strings.Count(fits.SRT(nil), "\n") != 2 { // 14 emoji are 41 units: one line
		t.Errorf("14 emoji wrapped: %q", fits.SRT(nil))
	}
}

func TestWordsSplitOnJavaScriptWhitespace(t *testing.T) {
	tr := transcriptWith(t, list(one("", 0, 1000, "a\uFEFFb c\u0085d\u3000e\u2003f")), "")
	if got, want := tr.SRT(nil), "1\n00:00:00,000 --> 00:00:01,000\na b c\u0085d e f"; got != want {
		t.Errorf("SRT = %q, want %q", got, want)
	}
}

func TestCaptionsSplitTheSpanByCharacterShareAndCapEveryCue(t *testing.T) {
	line30 := strings.Repeat("a", 30)
	text := line30 + " " + strings.Repeat("b", 30) + " " + strings.Repeat("c", 30)
	// chunks "aaa..\nbbb.." (61 units) and "ccc.." (30): 9100 ms split 6100 / 3000.
	got := transcriptWith(t, list(one("A", 0, 9100, text)), "").SRT(nil)
	want := "1\n00:00:00,000 --> 00:00:06,100\nSpeaker A: " + line30 + "\n" + strings.Repeat("b", 30) +
		"\n\n2\n00:00:06,100 --> 00:00:09,100\nSpeaker A: " + strings.Repeat("c", 30)
	if got != want {
		t.Errorf("SRT = %q\nwant %q", got, want)
	}
	// 91 s: both cues would run longer than 7 s, and the last one is capped too, from its own start.
	long := transcriptWith(t, list(one("", 10000, 101000, text)), "").SRT(nil)
	if !strings.Contains(long, "00:00:10,000 --> 00:00:17,000") || !strings.Contains(long, "00:01:11,000 --> 00:01:18,000") {
		t.Errorf("cues were not capped at 7000 ms: %q", long)
	}
	short := transcriptWith(t, list(one("", 0, 20000, "just a few words")), "").VTT(nil)
	if !strings.Contains(short, "00:00:00.000 --> 00:00:07.000") {
		t.Errorf("a single cue of a long utterance is not capped: %q", short)
	}
}

func TestAnOverlongWordKeepsALineAndAnEmptyTextIsOneCue(t *testing.T) {
	word := strings.Repeat("w", 50)
	got := transcriptWith(t, list(one("", 0, 2000, "x "+word+" y")), "").SRT(nil)
	if want := "1\n00:00:00,000 --> 00:00:01,962\nx\n" + word + "\n\n2\n00:00:01,962 --> 00:00:02,000\ny"; got != want {
		t.Errorf("SRT = %q, want %q", got, want)
	}
	if cues := wrapIntoCues("  \u00a0\n ", 42, 2); len(cues) != 1 || cues[0] != "" {
		t.Errorf("blank text = %q", cues)
	}
	empty := transcriptWith(t, list(one("A", 0, 1000, "")), "")
	if got := empty.SRT(nil); got != "1\n00:00:00,000 --> 00:00:01,000\nSpeaker A: " {
		t.Errorf("empty utterance SRT = %q", got)
	}
	// the cue ends where it starts when the utterance ends before it began
	backwards := transcriptWith(t, list(one("", 5000, 4000, "hi")), "")
	if got := backwards.SRT(nil); got != "1\n00:00:05,000 --> 00:00:05,000\nhi" {
		t.Errorf("backwards utterance SRT = %q", got)
	}
}

func TestPlainCuesKeepTheUtteranceWhole(t *testing.T) {
	text := strings.Repeat("word ", 30)
	tr := transcriptWith(t, list(one("A", 0, 30000, text), one("A", 30000, 31000, "x")), `"speaker_names":{"A":"Ann"},`)
	got := tr.SRT(&FormatOptions{Captions: flag(false)})
	want := "1\n00:00:00,000 --> 00:00:30,000\nAnn: " + text + "\n\n2\n00:00:30,000 --> 00:00:31,000\nAnn: x"
	if got != want {
		t.Errorf("SRT = %q\nwant %q", got, want)
	}
	if again := tr.SRT(&FormatOptions{Captions: flag(true)}); again != tr.SRT(nil) {
		t.Error("Captions true differs from the default")
	}
}

func TestSRTAndVTTEscapeWhatTheirReadersTakeForSyntax(t *testing.T) {
	text := "a ---> b -- > c --> d\n\n  \nTom & Jerry <b>\r\nlast\rend"
	tr := transcriptWith(t, list(one("", 3_723_004, 3_730_000, text)), "")
	plain := &FormatOptions{Captions: flag(false)}
	if got, want := tr.SRT(plain), "1\n01:02:03,004 --> 01:02:10,000\na -> b -- > c -> d\nTom & Jerry <b>\nlast\nend"; got != want {
		t.Errorf("SRT = %q\nwant %q", got, want)
	}
	if got, want := tr.VTT(plain), "WEBVTT\n\n1\n01:02:03.004 --> 01:02:10.000\na ---&gt; b -- > c --&gt; d\nTom &amp; Jerry &lt;b>\nlast\nend\n"; got != want {
		t.Errorf("VTT = %q\nwant %q", got, want)
	}
	if got := transcriptWith(t, "[]", "").VTT(nil); got != "WEBVTT\n\n\n" {
		t.Errorf("empty VTT = %q", got)
	}
	if got := transcriptWith(t, "[]", "").SRT(nil); got != "" {
		t.Errorf("empty SRT = %q", got)
	}
}

func TestTextMergesTimestampsAndKeepsWhitespaceAsItCame(t *testing.T) {
	tr := transcriptWith(t, list(
		one("A", 0, 1000, "one"), one("A", 1000, 2000, "two"), one("B", 65_000, 66_000, "a\r\n  \nb"),
		one("", 3_661_000, 3_662_000, "no label"), one("A", 59_999, 60_000, "z"),
	), `"speaker_names":{"B":"Bea"},`)
	if got, want := tr.Text(nil), "Speaker A: one two\n\nBea: a\r\n  \nb\n\nno label\n\nSpeaker A: z"; got != want {
		t.Errorf("Text = %q\nwant %q", got, want)
	}
	if got, want := tr.Text(&TextOptions{Timestamps: true}), "[0:00] Speaker A: one two\n\n[1:05] Bea: a\r\n  \nb\n\n[1:01:01] no label\n\n[0:59] Speaker A: z"; got != want {
		t.Errorf("Text timestamps = %q\nwant %q", got, want)
	}
	if got, want := tr.Text(&TextOptions{Speakers: flag(false), Merge: flag(false)}), "one\n\ntwo\n\na\r\n  \nb\n\nno label\n\nz"; got != want {
		t.Errorf("Text plain = %q\nwant %q", got, want)
	}
	if got := transcriptWith(t, "[]", "").Text(nil); got != "" {
		t.Errorf("empty Text = %q", got)
	}
}

func TestFormattersUseTheRequestedLanguageAndFallBackToTheOriginal(t *testing.T) {
	var tr Transcript
	raw := `{"id":"x","created_at":1,"speaker_names":{"A":"Ann"},"body":{"original":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":1000,"text":"hello"}]},` +
		`"translations":{"es":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":1000,"text":"hola"}]}}}}`
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		t.Fatal(err)
	}
	if got := tr.Text(&TextOptions{Language: "es"}); got != "Ann: hola" {
		t.Errorf("es = %q", got)
	}
	if got := tr.Text(&TextOptions{Language: "fr"}); got != "Ann: hello" {
		t.Errorf("fr = %q", got)
	}
	if got := tr.VTT(&FormatOptions{Language: "es", Captions: flag(false)}); !strings.Contains(got, "Ann: hola") {
		t.Errorf("vtt es = %q", got)
	}
}

func TestTranscriptTimestampNumbersRespectIntegerBoundsAndFloorFractions(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	minInt := -maxInt - 1
	for _, field := range []string{"start_ms", "end_ms"} {
		for _, locale := range []string{"original", "fr", "cleaned"} {
			for _, tc := range []struct {
				name, value string
				want        int
				wantError   bool
			}{
				{"int-max", strconv.Itoa(maxInt), maxInt, false},
				{"int-min", strconv.Itoa(minInt), minInt, false},
				{"above-int-max", strconv.FormatUint(uint64(maxInt)+1, 10), 0, true},
				{"below-int-min", "-" + strconv.FormatUint(uint64(maxInt)+2, 10), 0, true},
				{"positive-overflow", "1e300", 0, true},
				{"negative-overflow", "-1e300", 0, true},
				{"positive-fraction", "1.9", 1, false},
				{"negative-fraction", "-0.1", -1, false},
				{"exponent", "1.9e1", 19, false},
				{"zero", "0", 0, false},
				{"quoted-number", `"1"`, 0, true},
				{"folded-key-quoted-number", `"1"`, 0, true},
				{"duplicate-invalid-first", `"1","` + field + `":1`, 0, true},
				{"nan", "NaN", 0, true},
				{"infinity", "Infinity", 0, true},
			} {
				t.Run(field+"/"+locale+"/"+tc.name, func(t *testing.T) {
					start, end := "0", "1000"
					if field == "start_ms" {
						start = tc.value
					} else {
						end = tc.value
					}
					utterance := `{"speaker":"A","start_ms":` + start + `,"end_ms":` + end + `,"text":"a"}`
					if tc.name == "folded-key-quoted-number" {
						utterance = strings.Replace(utterance, `"`+field+`"`, `"`+strings.ToUpper(field)+`"`, 1)
					}
					body := `{"id":"tr_1","created_at":1,"body":{"original":{"utterances":[` + utterance + `]}}}`
					if locale != "original" {
						body = `{"id":"tr_1","created_at":1,"body":{"original":{"utterances":[]},"translations":{"` + locale + `":{"utterances":[` + utterance + `]}}}}`
					}
					var got Transcript
					err := json.Unmarshal([]byte(body), &got)
					if tc.wantError {
						if err == nil {
							t.Fatal("invalid timestamp accepted")
						}
						return
					}
					if err != nil {
						t.Fatalf("valid timestamp %s rejected: %v", tc.value, err)
					}
					items := got.UtterancesFor(locale)
					if len(items) != 1 {
						t.Fatalf("utterances = %d, want 1", len(items))
					}
					value := items[0].StartMs
					if field == "end_ms" {
						value = items[0].EndMs
					}
					if value != tc.want {
						t.Errorf("timestamp = %d, want %d", value, tc.want)
					}
				})
			}
		}
	}
}

func TestNestedUtterancesKeepTheirOwnRawInEveryLanguage(t *testing.T) {
	for _, unknown := range []string{`null`, `""`, `"   "`, `"\t"`, `"\u2003"`, `false`, `0`, `[]`, `{"deeper":[1,null,"x"]}`} {
		t.Run(unknown, func(t *testing.T) {
			utterance := `{"speaker":"A","start_ms":0.9,"end_ms":10,"text":"hi","future":` + unknown + `}`
			data := []byte(`{"id":"tr_1","created_at":1,"speaker_names":{"A":"Ann"},"body":{"original":{"utterances":[` + utterance + `]},"translations":{"fr":{"utterances":[` + utterance + `]},"cleaned":{"utterances":[` + utterance + `]}}}}`)
			var transcript Transcript
			if err := json.Unmarshal(data, &transcript); err != nil {
				t.Fatal(err)
			}
			for i := range data {
				data[i] = ' '
			}
			for _, language := range []string{"original", "fr", "cleaned"} {
				items := transcript.UtterancesFor(language)
				if len(items) != 1 {
					t.Fatalf("%s utterances = %v", language, items)
				}
				if items[0].SpeakerName != "Ann" || items[0].StartMs != 0 || items[0].EndMs != 10 || items[0].Text != "hi" {
					t.Fatalf("%s utterance = %+v", language, items[0])
				}
				if raw := nestedRaw(t, items[0]); raw != utterance {
					t.Errorf("%s Raw = %s, want %s", language, raw, utterance)
				}
			}
		})
	}
}
