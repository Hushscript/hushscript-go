package hushscript

import (
	"io"
	"time"
)

// Defaults and limits of the transcribe pipeline (spec sections 6.1, 7.3, 8).
const (
	defaultConcurrency = 4
	maxConcurrency     = 8
	defaultWaitTimeout = time.Hour
)

// Bool returns a pointer to v, for the options that tell "not set" from false with a *bool:
// Speakers, AutoInsights, Captions and Merge. hushscript.Bool(false) turns one off.
func Bool(v bool) *bool { return &v }

// ProgressStage says what a Progress report is about.
type ProgressStage string

// The two stages of a transcription.
const (
	// StageUploading reports Percent, BytesSent and BytesTotal.
	StageUploading ProgressStage = "uploading"
	// StageProcessing reports State, "queued" or "processing".
	StageProcessing ProgressStage = "processing"
)

// Progress is one report to a TranscribeOptions.OnProgress callback. It is called from the goroutine
// that runs the call and from its upload workers, never concurrently with itself.
type Progress struct {
	Stage ProgressStage
	// Percent is the share of bytes the API has acknowledged, 0 to 100 (uploading only).
	Percent    int
	BytesSent  int64
	BytesTotal int64
	// State is "queued" or "processing" (processing only).
	State string
}

// TranscribeOptions are the options of Transcribe, TranscribeReader and their kin. The zero value
// transcribes with every default. A zero Concurrency, WaitTimeout or DurationSeconds means "unset";
// a negative, NaN or infinite one is an invalid_option error, and nothing is ever clamped.
type TranscribeOptions struct {
	// Title is the transcript's title, at most 300 bytes (the server truncates).
	Title string
	// Language forces the spoken language; "" auto-detects.
	Language string
	// Speakers turns speaker labels on or off; nil leaves the API's default.
	Speakers *bool
	// TranslateTo lists locales to translate into: +25% of the duration each, and speakers are forced on.
	TranslateTo []string
	// Keyterms biases transcription toward domain vocabulary: at most 1000 terms of at most 6 words.
	Keyterms []string
	// Prompt is free-text context.
	Prompt string
	// Medical transcribes as a medical dictation (billed double); Language must be en, es, fr or de.
	Medical bool
	// Multichannel transcribes each audio channel on its own and bills per channel, up to 8.
	Multichannel bool
	// AutoInsights overrides the account's automatic-Insights default; nil inherits it.
	AutoInsights *bool
	// MatterCode files the transcript under a matter (organization accounts), at most 64 characters.
	MatterCode string
	// DurationSeconds overrides the duration probe. It must be finite and positive, and 15 to 36000.
	// TranscribeStream requires it: a forward-only stream cannot be probed.
	DurationSeconds float64
	// IdempotencyKey defaults to a fresh UUID v4 per call. Pass the key an error carried to replay the same job.
	IdempotencyKey string
	// Concurrency is how many parts upload at once: 0 for the default 4, otherwise 1 to 8.
	Concurrency int
	// WaitTimeout is how long to wait for processing: 0 for the default hour. The job keeps running
	// when it runs out, and the error carries a resume token.
	WaitTimeout time.Duration
	// OnProgress receives upload and processing reports.
	//
	// OnProgress and OnResumeToken cannot return an error. A panic in one is the caller's own: the
	// SDK does not recover it or wrap it, and it leaves the call with the value it was raised with (a
	// panic in OnProgress on an upload worker stops the upload and is raised again, unchanged, on the
	// goroutine that made the call).
	OnProgress func(Progress)
	// OnResumeToken is called once, as soon as the job exists.
	OnResumeToken func(token string)
}

// ResumeOptions are the options of Resume. The file is needed only while the job is still uploading;
// give it as Path, as Reader with Size, or as Stream; only one of them.
type ResumeOptions struct {
	// Path is the original file.
	Path string
	// Reader and Size are the original file as a random-access reader of exactly Size bytes.
	Reader io.ReaderAt
	Size   int64
	// Stream is the original file as a forward-only reader, positioned at the start of the file. Its
	// size is the one the resume token records, so there is no Size to give; it is read in order, once.
	Stream io.Reader
	// Concurrency, WaitTimeout and OnProgress are as in TranscribeOptions.
	Concurrency int
	WaitTimeout time.Duration
	OnProgress  func(Progress)
}

// WaitOptions are the options of Jobs.Wait.
type WaitOptions struct {
	// WaitTimeout is how long to wait: 0 for the default hour.
	WaitTimeout time.Duration
	OnProgress  func(Progress)
}

func (c Client) pipeline() (*core, error) {
	if c.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	return c.core, nil
}

const notBuiltMessage = "This Client was not built by hushscript.New."
