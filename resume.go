package hushscript

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
)

// resumeState is what a resume token holds: the job, the key that replays its create, and the file
// it started with, by size and declared duration (spec 6.1).
type resumeState struct {
	JobID           string  `json:"jobId"`
	IdempotencyKey  string  `json:"idempotencyKey"`
	Size            int64   `json:"size"`
	DurationSeconds float64 `json:"durationSeconds"`
}

const resumeTokenVersion = 1

// encodeResumeToken is base64url (no padding) of {v:1, jobId, idempotencyKey, size, durationSeconds},
// the same bytes every Hushscript SDK writes.
func encodeResumeToken(state resumeState) string {
	body, err := json.Marshal(struct {
		V int `json:"v"`
		resumeState
	}{resumeTokenVersion, state})
	if err != nil { // a struct of strings and numbers cannot fail, unless the duration is not finite
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(body)
}

var errBadToken = errors.New("the resume token is not valid")

func decodeResumeToken(token string) (resumeState, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(token, "="))
	if err != nil {
		return resumeState{}, errBadToken
	}
	var body struct {
		V int `json:"v"`
		resumeState
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return resumeState{}, errBadToken
	}
	state := body.resumeState
	if body.V != resumeTokenVersion || !safeSegment(state.JobID) || state.IdempotencyKey == "" || state.Size < 1 ||
		!(state.DurationSeconds >= minDurationSeconds && math.Ceil(state.DurationSeconds) <= maxDurationSeconds) {
		return resumeState{}, errBadToken
	}
	return state, nil
}

func (c *core) jobMissing() *Error {
	return newSDKError(KindResumeMismatch, "resume_job_missing",
		"The job behind this resume token no longer exists, so it cannot be resumed.", nil, c.cfg.redactor(), hintInputs{})
}

// Resume picks a transcription up from the token an error carried. It reads the job first. A job past
// uploading is waited for and fetched without the file; a job still uploading replays its create with
// the original key (which returns the same job, with no second hold) and needs the file, as Path or as
// Reader and Size, of the same size as the original. A job that no longer exists is a ResumeMismatch
// error with code resume_job_missing, and nothing is replayed.
func (c Client) Resume(ctx context.Context, token string, opts *ResumeOptions) (result *Transcript, err error) {
	p, err := c.pipeline()
	if err != nil {
		return nil, err
	}
	red := p.cfg.redactor()
	state, decodeErr := decodeResumeToken(token)
	if decodeErr != nil {
		return nil, newSDKError(KindConfiguration, "config_invalid", "The resume token is not valid.", nil, red, hintInputs{})
	}
	ann := &annotator{core: p, key: state.IdempotencyKey, token: token}
	defer func() {
		if err != nil {
			result, err = nil, ann.annotate(ctx, err)
		}
	}()
	if opts == nil {
		opts = &ResumeOptions{}
	}
	concurrency, waitTimeout, err := runtimeOptions(red, opts.Concurrency, opts.WaitTimeout, 0)
	if err != nil {
		return nil, err
	}
	src, err := resumeSource(opts, red)
	if err != nil {
		return nil, err
	}
	if size, known := knownSize(src, opts.Path); known && size != state.Size {
		return nil, fileMismatch(red, size, state.Size)
	}
	r := &run{core: p, state: state, token: token, src: src, concurrency: concurrency, waitTimeout: waitTimeout, onProgress: opts.OnProgress}
	job, err := p.readJob(ctx, state.JobID)
	if err != nil {
		var failure *Error
		if errors.As(err, &failure) && failure.Kind == KindNotFound {
			return nil, p.jobMissing()
		}
		return nil, err
	}
	if job.State != "uploading" {
		return r.finish(ctx, job.State, nil, job)
	}
	if src == nil && opts.Path != "" {
		// The path is opened only now: a job past uploading never needed the file.
		if src, err = openPath(opts.Path, red); err != nil {
			return nil, err
		}
		defer src.close()
		if src.size != state.Size {
			return nil, fileMismatch(red, src.size, state.Size)
		}
		r.src = src
	}
	if src == nil {
		return nil, newSDKError(KindResumeMismatch, "resume_file_required",
			"The upload is not finished, so resuming needs the original file.", nil, red, hintInputs{})
	}
	if opts.OnProgress != nil {
		opts.OnProgress(Progress{Stage: StageUploading, BytesTotal: src.size})
	}
	created, status, err := p.createUpload(ctx, createBody{
		SizeBytes:       state.Size,
		DurationSeconds: int(math.Ceil(state.DurationSeconds)),
		IdempotencyKey:  state.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if created.JobId != state.JobID {
		return nil, p.jobMissing()
	}
	plan, failure := p.planOf(created, status)
	if failure != nil {
		return nil, failure
	}
	return r.finish(ctx, created.State, plan, nil)
}

func fileMismatch(red redactor, got, want int64) *Error {
	return newSDKError(KindResumeMismatch, "resume_file_mismatch",
		"The file is "+formatInt(got)+" bytes; the upload started with "+formatInt(want)+" bytes.", nil, red, hintInputs{})
}

// knownSize is the size of the file the caller gave, without opening a Path: a Path that cannot be
// stat'd is left for the moment it is needed, so a resume of a finished job never fails on it.
func knownSize(src *fileSource, path string) (int64, bool) {
	if src != nil {
		return src.size, true
	}
	if path == "" {
		return 0, false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, false
	}
	return info.Size(), true
}

// resumeSource is the Reader the caller gave, if any. A Path is not opened here (see Resume).
func resumeSource(opts *ResumeOptions, red redactor) (*fileSource, error) {
	switch {
	case opts.Path != "" && opts.Reader != nil:
		return nil, invalidOption(red, "Give the original file as Path or as Reader, not both.", nil)
	case opts.Path != "":
		return nil, nil
	case opts.Reader != nil:
		if opts.Size <= 0 {
			return nil, invalidOption(red, "Size must be the file's size in bytes, above 0.", nil)
		}
		return &fileSource{r: opts.Reader, size: opts.Size}, nil
	}
	return nil, nil
}
