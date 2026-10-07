package hushscript

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Hushscript/hushscript-go/generated"
	"github.com/Hushscript/hushscript-go/internal/probe"
)

const (
	maxFileBytes       = 5_000_000_000
	minDurationSeconds = 15
	maxDurationSeconds = 36_000
	maxKeyterms        = 1000
	maxKeytermWords    = 6
	maxMatterCodeChars = 64
	minKeyLength       = 16
	maxKeyLength       = 128
)

func formatInt(v int64) string { return strconv.FormatInt(v, 10) }

func invalidOption(red redactor, message string, cause error) *Error {
	return newSDKError(KindPreflight, "invalid_option", message, cause, red, hintInputs{})
}

// safeSegment reports whether id can stand as one path segment of a request: "", "." and ".." would
// collapse into another route (GET /v1/jobs/.. is GET /v1), and bytes that are not UTF-8 have no
// agreed percent-encoding. Everything else is percent-encoded by the generated client.
func safeSegment(id string) bool {
	return id != "" && id != "." && id != ".." && utf8.ValidString(id)
}

// checkID is the invalid_option error for an id that cannot be a path segment, raised before any
// request. name is the option as the caller knows it.
func checkID(red redactor, name, id string) error {
	if safeSegment(id) {
		return nil
	}
	return invalidOption(red, name+` must be a non-empty id that is not "." or "..", in valid UTF-8.`, nil)
}

// newIdempotencyKey is a UUID v4 string: 36 characters, valid for uploads (16 to 128 bytes of 0x21 to 0x7E)
// and purchases alike (spec 7.3).
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil { // the system's random source failing is not recoverable
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// runtimeOptions validates what every call takes: concurrency, wait timeout and a duration override.
// Zero means "unset" for each; anything else outside its range is invalid_option, never clamped.
func runtimeOptions(red redactor, concurrency int, waitTimeout time.Duration, duration float64) (int, time.Duration, error) {
	if concurrency == 0 {
		concurrency = defaultConcurrency
	}
	if concurrency < 1 || concurrency > maxConcurrency {
		return 0, 0, invalidOption(red, "Concurrency must be a whole number from 1 to 8.", nil)
	}
	timeout, err := waitTimeoutOf(red, waitTimeout)
	if err != nil {
		return 0, 0, err
	}
	if duration != 0 && (math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0) {
		return 0, 0, invalidOption(red, "DurationSeconds must be a finite number of seconds, above 0.", nil)
	}
	return concurrency, timeout, nil
}

func waitTimeoutOf(red redactor, waitTimeout time.Duration) (time.Duration, error) {
	switch {
	case waitTimeout == 0:
		return defaultWaitTimeout, nil
	case waitTimeout < 0:
		return 0, invalidOption(red, "WaitTimeout must be a positive duration.", nil)
	}
	return waitTimeout, nil
}

// checkKey is spec 7.3: an upload key is 16 to 128 bytes of 0x21 to 0x7E (printable ASCII, no space).
func checkKey(red redactor, key string) error {
	ok := len(key) >= minKeyLength && len(key) <= maxKeyLength
	for i := 0; ok && i < len(key); i++ {
		ok = key[i] >= 0x21 && key[i] <= 0x7e
	}
	if !ok {
		return invalidOption(red, "IdempotencyKey must be 16 to 128 printable ASCII characters, without spaces.", nil)
	}
	return nil
}

func checkOptions(red redactor, opts *TranscribeOptions) error {
	if opts.Medical {
		switch opts.Language {
		case "en", "es", "fr", "de":
		default:
			return invalidOption(red, "Medical needs Language to be one of en, es, fr or de.", nil)
		}
	}
	if len(opts.Keyterms) > maxKeyterms {
		return invalidOption(red, "Keyterms takes at most 1000 terms.", nil)
	}
	for _, term := range opts.Keyterms {
		if len(strings.Fields(term)) > maxKeytermWords {
			return invalidOption(red, "Each keyterm is at most 6 words.", nil)
		}
	}
	if len([]rune(opts.MatterCode)) > maxMatterCodeChars {
		return invalidOption(red, "MatterCode is at most 64 characters.", nil)
	}
	return nil
}

// annotator adds what a caller needs to the errors of one call: the idempotency key once the create
// was sent, the resume token once the job id is known (spec 7.3), the SDK's own abort error for a
// bare context error, and, once a request that carries a key or token was sent, unexpected_error for
// any other failure that is not an SDK error (spec 9.2).
type annotator struct {
	core  *core
	key   string
	token string
}

func (a *annotator) annotate(ctx context.Context, err error) error {
	failure, ok := err.(*Error) //nolint:errorlint // the pipeline returns its own errors unwrapped
	if !ok {
		switch {
		case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			failure = a.core.abortError(err)
		case a.key != "" || a.token != "":
			failure = a.core.unexpectedError(err)
		default:
			failure = a.core.requestNotBuilt(err)
		}
	}
	if failure.IdempotencyKey == "" {
		failure.IdempotencyKey = a.key
	}
	if failure.ResumeToken == "" {
		failure.ResumeToken = a.token
	}
	return failure
}

// requestNotBuilt is the error for a request the SDK could not even build, which only a base URL or a
// path that cannot form a URL causes.
func (c *core) requestNotBuilt(cause error) *Error {
	return newSDKError(KindConfiguration, "config_invalid", "The request could not be built.", cause, c.cfg.redactor(), hintInputs{})
}

// unexpectedError is the error for a failure of the SDK's own after a request was sent that fits no
// other class (spec 9.2). Its kind is KindUnexpected, the base class, with no status. The original
// rides along as a redacted cause.
func (c *core) unexpectedError(cause error) *Error {
	return newSDKError(KindUnexpected, "unexpected_error", "The SDK failed unexpectedly after the request was sent.", cause, c.cfg.redactor(), hintInputs{})
}

// Transcribe uploads the file at path, waits for it to be transcribed and returns the transcript. The
// format is decided by the file's bytes, not its name. A nil opts uses every default.
//
// Every error raised after the upload was created carries the idempotency key (Error.IdempotencyKey)
// and, once the job exists, a resume token (Error.ResumeToken); a cancelled ctx is a KindAbort error.
func (c Client) Transcribe(ctx context.Context, path string, opts *TranscribeOptions) (*Transcript, error) {
	p, err := c.pipeline()
	if err != nil {
		return nil, err
	}
	src, err := openPath(path, p.cfg.redactor())
	if err != nil {
		return nil, err
	}
	defer src.close()
	return p.transcribe(ctx, src, opts)
}

// TranscribeReader is Transcribe for a random-access reader of exactly size bytes. It reads each part
// at its own offset, never the whole file at once; a reader that holds more or fewer bytes than size
// is an UploadError with code stream_size_mismatch, before the upload is completed.
func (c Client) TranscribeReader(ctx context.Context, r io.ReaderAt, size int64, opts *TranscribeOptions) (*Transcript, error) {
	p, err := c.pipeline()
	if err != nil {
		return nil, err
	}
	red := p.cfg.redactor()
	if r == nil {
		return nil, invalidOption(red, "The reader cannot be nil.", nil)
	}
	if size <= 0 {
		return nil, invalidOption(red, "The size must be the exact number of bytes in the reader, above 0.", nil)
	}
	return p.transcribe(ctx, &fileSource{r: r, size: size}, opts)
}

// run is the state of one transcribe or resume call that its stages share.
type run struct {
	core        *core
	state       resumeState
	token       string
	src         *fileSource
	concurrency int
	waitTimeout time.Duration
	onProgress  func(Progress)
}

type createBody struct {
	SizeBytes            int64                 `json:"size_bytes"`
	DurationSeconds      int                   `json:"duration_seconds"`
	IdempotencyKey       string                `json:"idempotency_key"`
	Title                string                `json:"title,omitempty"`
	MatterCode           string                `json:"matter_code,omitempty"`
	TranscriptionOptions *transcriptionOptions `json:"transcription_options,omitempty"`
}

type transcriptionOptions struct {
	LanguageCode         string   `json:"language_code,omitempty"`
	SpeakerDetection     *bool    `json:"speaker_detection,omitempty"`
	TranslationTargets   []string `json:"translation_targets,omitempty"`
	KeytermsPrompt       []string `json:"keyterms_prompt,omitempty"`
	Prompt               string   `json:"prompt,omitempty"`
	MedicalMode          bool     `json:"medical_mode,omitempty"`
	Multichannel         bool     `json:"multichannel,omitempty"`
	AutoGenerateInsights *bool    `json:"auto_generate_insights,omitempty"`
}

func bodyFrom(opts *TranscribeOptions, size int64, declared int, key string) createBody {
	body := createBody{SizeBytes: size, DurationSeconds: declared, IdempotencyKey: key, Title: opts.Title, MatterCode: opts.MatterCode}
	options := transcriptionOptions{
		LanguageCode: opts.Language, SpeakerDetection: opts.Speakers, TranslationTargets: opts.TranslateTo,
		KeytermsPrompt: opts.Keyterms, Prompt: opts.Prompt, MedicalMode: opts.Medical, Multichannel: opts.Multichannel,
		AutoGenerateInsights: opts.AutoInsights,
	}
	if options.LanguageCode != "" || options.SpeakerDetection != nil || len(options.TranslationTargets) > 0 || len(options.KeytermsPrompt) > 0 ||
		options.Prompt != "" || options.MedicalMode || options.Multichannel || options.AutoGenerateInsights != nil {
		body.TranscriptionOptions = &options
	}
	return body
}

// createUpload sends POST /v1/uploads. The doer paces it and replays it on a lost answer, because the
// body carries the idempotency key.
func (c *core) createUpload(ctx context.Context, body createBody) (created *generated.UploadCreateResponse, status int, err error) {
	const route = "POST /v1/uploads"
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, 0, c.requestNotBuilt(err)
	}
	response, err := c.api.CreateUploadWithBodyWithResponse(ctx, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, c.readFailed(err, route)
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := c.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "job_id", "state"); failure != nil {
		return nil, 0, failure
	}
	if !safeSegment(parsed.JobId) {
		return nil, 0, c.badResponse(route, response.StatusCode())
	}
	if upload := parsed.Upload; upload != nil && !partRowsUsable(upload.PartUrls) {
		return nil, 0, c.badResponse(route, response.StatusCode())
	}
	return parsed, response.StatusCode(), nil
}

func (c *core) transcribe(ctx context.Context, src *fileSource, opts *TranscribeOptions) (result *Transcript, err error) {
	if opts == nil {
		opts = &TranscribeOptions{}
	}
	red := c.cfg.redactor()
	ann := &annotator{core: c}
	defer func() {
		if err != nil {
			result, err = nil, ann.annotate(ctx, err)
		}
	}()
	concurrency, waitTimeout, err := runtimeOptions(red, opts.Concurrency, opts.WaitTimeout, opts.DurationSeconds)
	if err != nil {
		return nil, err
	}
	if opts.IdempotencyKey != "" {
		err = checkKey(red, opts.IdempotencyKey)
		if err != nil {
			return nil, err
		}
	}
	err = checkOptions(red, opts)
	if err != nil {
		return nil, err
	}
	declared, err := c.preflight(ctx, src, opts)
	if err != nil {
		return nil, err
	}
	key := opts.IdempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	if opts.OnProgress != nil {
		opts.OnProgress(Progress{Stage: StageUploading, BytesTotal: src.size})
	}
	ann.key = key // from here the create may have reached the server
	created, status, err := c.createUpload(ctx, bodyFrom(opts, src.size, declared, key))
	if err != nil {
		return nil, err
	}
	state := resumeState{JobID: created.JobId, IdempotencyKey: key, Size: src.size, DurationSeconds: float64(declared)}
	token := encodeResumeToken(state)
	ann.token = token
	if opts.OnResumeToken != nil {
		opts.OnResumeToken(token)
	}
	plan, failure := c.planOf(created, status)
	if failure != nil {
		return nil, failure // after the token is known: the job exists, so the error carries the way back to it
	}
	r := &run{core: c, state: state, token: token, src: src, concurrency: concurrency, waitTimeout: waitTimeout, onProgress: opts.OnProgress}
	return r.finish(ctx, created.State, plan, nil)
}

// preflight is everything the SDK can refuse before a hold is placed (spec 8.1), in the spec's order.
// It returns the duration to declare: the probed or supplied seconds, rounded up.
func (c *core) preflight(ctx context.Context, src *fileSource, opts *TranscribeOptions) (int, error) {
	red := c.cfg.redactor()
	format := probe.SniffAt(src.r, src.size)
	if format == probe.FormatUnknown {
		return 0, newSDKError(KindUnsupportedMedia, "unsupported_media", "The file is not in a supported audio format.", nil, red, hintInputs{})
	}
	if src.size < 1 || src.size > maxFileBytes {
		return 0, newSDKError(KindFileLimit, "size_out_of_range",
			"The file is "+formatInt(src.size)+" bytes; the limit is 1 byte to 5000000000 bytes.", nil, red, hintInputs{})
	}
	seconds := opts.DurationSeconds
	if seconds == 0 {
		probed, err := probe.Duration(src.r, src.size, format)
		if err != nil {
			return 0, newSDKError(KindUnsupportedMedia, "duration_unreadable", "The file's duration could not be read.", err, red, hintInputs{})
		}
		seconds = probed
	}
	if seconds < minDurationSeconds || math.Ceil(seconds) > maxDurationSeconds {
		return 0, newSDKError(KindFileLimit, "duration_out_of_range",
			"The duration is "+strconv.FormatFloat(seconds, 'f', -1, 64)+" s; it must be 15 s to 10 hours.", nil, red, hintInputs{})
	}
	declared := int(math.Ceil(seconds))

	const accountRoute = "GET /v1/account"
	account, err := c.api.GetAccountWithResponse(ctx)
	if err != nil {
		return 0, c.readFailed(err, accountRoute)
	}
	parsed := decodedBody(account.JSON200, account.HTTPResponse, account.Body)
	if failure := c.unusable(accountRoute, account.HTTPResponse, parsed != nil, account.Body,
		"balance_seconds", "limits.used_today_seconds", "limits.max_daily_seconds"); failure != nil {
		return 0, failure
	}
	needed := billableEstimate(declared, opts)
	balance := parsed.BalanceSeconds
	if balance < needed {
		failure := newSDKError(KindInsufficientBalance, "insufficient_balance",
			"The balance is "+strconv.Itoa(balance)+" s; this file needs about "+strconv.Itoa(needed)+" s.", nil, red,
			hintInputs{neededSeconds: some(float64(needed)), balanceSeconds: some(float64(max(balance, 0)))})
		failure.Details = map[string]any{"needed_seconds": float64(needed), "balance_seconds": float64(balance)}
		return 0, failure
	}
	limits := parsed.Limits
	if limits.UsedTodaySeconds+declared > limits.MaxDailySeconds {
		failure := newSDKError(KindDailyLimit, "daily_limit",
			"This upload would pass the daily limit ("+strconv.Itoa(limits.UsedTodaySeconds)+" of "+strconv.Itoa(limits.MaxDailySeconds)+" s used).", nil, red,
			hintInputs{usedSeconds: some(float64(limits.UsedTodaySeconds)), maxSeconds: some(float64(limits.MaxDailySeconds))})
		failure.Details = map[string]any{"used_seconds": float64(limits.UsedTodaySeconds), "max_seconds": float64(limits.MaxDailySeconds)}
		return 0, failure
	}
	return declared, nil
}

// billableEstimate is the duration times the multipliers a hold would carry (spec 8.1). Multichannel
// counts once, because the channel count is unknown before the upload.
func billableEstimate(seconds int, opts *TranscribeOptions) int {
	factor := 1.0 + 0.25*float64(len(opts.TranslateTo))
	if opts.Medical {
		factor++
	}
	return int(math.Ceil(float64(seconds) * factor))
}

// finish takes a job from the state its create or its read reported to a transcript. known is the job
// as Resume just read it, for a job that is not uploading.
func (r *run) finish(ctx context.Context, jobState string, plan *uploadPlan, known *Job) (*Transcript, error) {
	c := r.core
	finished := known
	if jobState == "uploading" {
		if r.src == nil {
			return nil, newSDKError(KindResumeMismatch, "resume_file_required",
				"The upload is not finished, so resuming needs the original file.", nil, c.cfg.redactor(), hintInputs{})
		}
		next, err := r.upload(ctx, plan)
		if err != nil {
			return nil, err
		}
		jobState, finished = next, nil
	}
	if jobState == "done" || jobState == "failed" {
		// The complete response has no transcript id: one job read gives it, with no poll loop.
		if finished == nil {
			job, err := c.readJob(ctx, r.state.JobID)
			if err != nil {
				return nil, err
			}
			finished = job
		}
		return c.fetchTranscript(ctx, finished)
	}
	if r.onProgress != nil && (jobState == "queued" || jobState == "processing") {
		r.onProgress(Progress{Stage: StageProcessing, State: jobState})
	}
	job, err := c.waitForJob(ctx, r.state.JobID, r.waitTimeout, r.onProgress, r.token)
	if err != nil {
		return nil, err
	}
	return c.fetchTranscript(ctx, job)
}
