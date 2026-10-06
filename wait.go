package hushscript

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

const (
	pollStart   = 5 * time.Second
	pollFactor  = 1.5
	pollCeiling = 15 * time.Second
)

func jobFromAPI(raw *generated.Job) *Job {
	job := &Job{ID: raw.Id, State: raw.State}
	if raw.TranscriptId != nil {
		job.TranscriptID = *raw.TranscriptId
	}
	if raw.Error != nil {
		job.Error = *raw.Error
	}
	if raw.FailureKind != nil {
		job.FailureKind = string(*raw.FailureKind)
	}
	return job
}

func (j *Job) finished() bool { return j.State == "done" || j.State == "failed" }

func (c *core) readJob(ctx context.Context, id string) (*Job, error) {
	response, err := c.api.GetJobWithResponse(ctx, id)
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, c.badResponse("GET /v1/jobs/{id}")
	}
	return jobFromAPI(response.JSON200), nil
}

func (c *core) badResponse(route string) *Error {
	return newSDKError(KindAPI, "bad_response", route+" answered without a body the SDK can read.", nil, c.cfg.redactor(), hintInputs{})
}

// sleep is clock.Sleep with a cancellation turned into the SDK's own abort error.
func (c *core) sleep(ctx context.Context, d time.Duration) error {
	if err := c.cfg.clock.Sleep(ctx, d); err != nil {
		return c.abortError(err)
	}
	return nil
}

func (c *core) abortError(cause error) *Error {
	return newSDKError(KindAbort, "aborted", "The request was cancelled.", cause, c.cfg.redactor(), hintInputs{})
}

// waitForJob polls the job at 5 s, times 1.5 up to 15 s, until it is done or failed (spec 8.6). Running
// out of timeout is a WaitTimeout error; the job keeps running, and token (when there is one) lets
// Resume pick it up.
func (c *core) waitForJob(ctx context.Context, id string, timeout time.Duration, onProgress func(Progress), token string) (*Job, error) {
	clock := c.cfg.clock
	deadline := clock.Now().Add(timeout)
	interval := pollStart
	for {
		job, err := c.readJob(ctx, id)
		if err != nil {
			return nil, err
		}
		if job.finished() {
			return job, nil
		}
		if onProgress != nil && (job.State == "queued" || job.State == "processing") {
			onProgress(Progress{Stage: StageProcessing, State: job.State})
		}
		remaining := deadline.Sub(clock.Now())
		if remaining <= 0 {
			failure := newSDKError(KindWaitTimeout, "wait_timeout",
				"The job is still "+job.State+" after "+timeout.String()+"; it keeps running.", nil, c.cfg.redactor(), hintInputs{})
			failure.ResumeToken = token
			return nil, failure
		}
		if err := c.sleep(ctx, min(interval, remaining)); err != nil {
			return nil, err
		}
		interval = min(time.Duration(float64(interval)*pollFactor), pollCeiling)
	}
}

// failedJob is the error for a job that ended in failure.
func (c *core) failedJob(job *Job) *Error {
	red := c.cfg.redactor()
	message := red.apply(job.Error)
	if message == "" {
		message = "The job failed."
	}
	failure := newSDKError(KindJobFailed, "job_failed", message, nil, red, hintInputs{})
	failure.JobID = job.ID
	failure.JobError = red.apply(job.Error)
	if job.FailureKind == "media_unreadable" || job.FailureKind == "media_undecodable" {
		failure.FailureKind = job.FailureKind
	}
	return failure
}

// fetchTranscript reads a finished job's transcript.
func (c *core) fetchTranscript(ctx context.Context, job *Job) (*Transcript, error) {
	if job.State == "failed" {
		return nil, c.failedJob(job)
	}
	if job.TranscriptID == "" {
		return nil, newSDKError(KindUpload, "transcript_missing", "The job is done but has no transcript id.", nil, c.cfg.redactor(), hintInputs{})
	}
	response, err := c.api.GetTranscriptWithResponse(ctx, job.TranscriptID)
	if err != nil {
		return nil, err
	}
	var head struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Language *string `json:"language_detected"`
		Duration float64 `json:"duration_seconds"`
	}
	if len(response.Body) == 0 || json.Unmarshal(response.Body, &head) != nil || head.ID == "" {
		return nil, c.badResponse("GET /v1/transcripts/{id}")
	}
	transcript := &Transcript{ID: head.ID, Title: head.Title, DurationSeconds: head.Duration, Raw: json.RawMessage(bytes.Clone(response.Body))}
	if head.Language != nil {
		transcript.Language = *head.Language
	}
	return transcript, nil
}

// Wait polls the job until it is done or failed and returns it either way; a failed job is a result
// here, not an error. Running out of WaitTimeout is a WaitTimeout error without a resume token.
func (j JobsService) Wait(ctx context.Context, id string, opts *WaitOptions) (*Job, error) {
	if j.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	if opts == nil {
		opts = &WaitOptions{}
	}
	red := j.core.cfg.redactor()
	timeout, err := waitTimeoutOf(red, opts.WaitTimeout)
	if err != nil {
		return nil, err
	}
	job, err := j.core.waitForJob(ctx, id, timeout, opts.OnProgress, "")
	if err != nil {
		return nil, (&annotator{core: j.core}).annotate(ctx, err)
	}
	return job, nil
}
