package hushscript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	pollStart   = 5 * time.Second
	pollFactor  = 1.5
	pollCeiling = 15 * time.Second
)

func (j *Job) finished() bool { return j.State == "done" || j.State == "failed" }

func (c *core) readJob(ctx context.Context, id string) (*Job, error) {
	const route = "GET /v1/jobs/{id}"
	response, err := c.api.GetJobWithResponse(ctx, id)
	if err != nil {
		return nil, c.readFailed(err, route)
	}
	if failure := c.unusable(route, response.HTTPResponse, response.JSON200 != nil, response.Body, "id", "state", "created_at"); failure != nil {
		return nil, failure
	}
	var body jobBody
	if json.Unmarshal(response.Body, &body) != nil {
		return nil, c.badResponse(route, response.StatusCode())
	}
	return body.job(), nil
}

// badResponse is the error for a 2xx answer the SDK cannot use: an ApiError with the status the API
// really answered with.
func (c *core) badResponse(route string, status int) *Error {
	failure := newSDKError(KindAPI, "bad_response", route+" answered without a body the SDK can read.", nil, c.cfg.redactor(), hintInputs{})
	failure.Status = status
	return failure
}

// readFailed turns a failure to decode a 2xx body (a malformed or mistyped JSON document) into
// bad_response. The generated parser decodes 200 bodies only, so that is the status. Anything else
// (the doer's own *Error, a cancellation) is returned as it is.
func (c *core) readFailed(err error, route string) error {
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typed) {
		return c.badResponse(route, http.StatusOK)
	}
	return err
}

// unusable returns bad_response when the answer decoded to nothing (an unexpected content type) or
// to a body without the fields the SDK relies on: a JSON null or `{}` decodes without error into a
// zero value that would otherwise be taken for a real answer. Keys are dotted paths into objects.
func (c *core) unusable(route string, response *http.Response, decoded bool, body []byte, required ...string) *Error {
	status := http.StatusOK
	if response != nil {
		status = response.StatusCode
	}
	if !decoded || !hasFields(body, required) {
		return c.badResponse(route, status)
	}
	return nil
}

// rowsHave reports whether the array at key holds only objects that have every field, none of them
// null: a row without one would otherwise decode into a silent zero.
func rowsHave(body []byte, key string, fields ...string) bool {
	var root map[string]json.RawMessage
	var rows []json.RawMessage
	if json.Unmarshal(body, &root) != nil || json.Unmarshal(root[key], &rows) != nil {
		return false
	}
	for _, row := range rows {
		if !hasFields(row, fields) {
			return false
		}
	}
	return true
}

// hasFields reports whether body is a JSON object holding every path, none of them null.
func hasFields(body []byte, paths []string) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return false
	}
	for _, path := range paths {
		object := root
		parts := strings.Split(path, ".")
		for i, key := range parts {
			value, ok := object[key]
			if !ok || string(bytes.TrimSpace(value)) == "null" {
				return false
			}
			if i < len(parts)-1 {
				object = nil
				if json.Unmarshal(value, &object) != nil || object == nil {
					return false
				}
			}
		}
	}
	return true
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
	return c.readTranscript(ctx, job.TranscriptID)
}

// readTranscript reads one transcript by id.
func (c *core) readTranscript(ctx context.Context, id string) (*Transcript, error) {
	const route = "GET /v1/transcripts/{id}"
	response, err := c.api.GetTranscriptWithResponse(ctx, id)
	if err != nil {
		return nil, c.readFailed(err, route)
	}
	if failure := c.unusable(route, response.HTTPResponse, response.JSON200 != nil, response.Body, transcriptRequired...); failure != nil {
		return nil, failure
	}
	transcript, err := parseTranscript(response.Body)
	if err != nil {
		return nil, c.badResponse(route, response.StatusCode())
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
