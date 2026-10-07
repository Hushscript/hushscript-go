package hushscript

import (
	"context"
	"encoding/json"
	"iter"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

// JobOptions is what a job was submitted with, from the API's content-free summary: it says whether
// keyterms and a prompt were set, never what they were.
type JobOptions struct {
	// LanguageCode is the forced language; "" when the language was auto-detected.
	LanguageCode       string
	SpeakerDetection   bool
	MedicalMode        bool
	Multichannel       bool
	TranslationTargets []string
	// Keyterms and Prompt say whether they were set.
	Keyterms bool
	Prompt   bool
	// AutoGenerateInsights is the upload's override; nil when it used the account default.
	AutoGenerateInsights *bool
}

// Job is a processing job as the API reports it (spec 6.4). The private-result and from-url fetch
// fields are left out; Client.API still returns them.
type Job struct {
	ID string
	// State is uploading, fetching, queued, processing, done or failed; a state the SDK does not know
	// yet passes through as the API's own string.
	State string
	// TranscriptID is set once the job is done.
	TranscriptID string
	// Error is the failure text of a failed job, opaque.
	Error string
	// FailureKind is "media_unreadable" or "media_undecodable" when the file itself could not be read.
	FailureKind string
	Mode        string
	// DeclaredSeconds, VerifiedSeconds, ActualSeconds and BillableSeconds are nil while unknown.
	DeclaredSeconds *int
	VerifiedSeconds *int
	ActualSeconds   *int
	BillableSeconds *int
	// RetryOf is the id of the failed job this one retries; "" when it retries none.
	RetryOf   string
	CreatedAt time.Time
	Options   JobOptions
}

type jobBody struct {
	ID              string  `json:"id"`
	State           string  `json:"state"`
	TranscriptID    *string `json:"transcript_id"`
	Error           *string `json:"error"`
	FailureKind     *string `json:"failure_kind"`
	Mode            string  `json:"mode"`
	DeclaredSeconds *int    `json:"declared_seconds"`
	VerifiedSeconds *int    `json:"verified_seconds"`
	ActualSeconds   *int    `json:"actual_seconds"`
	BillableSeconds *int    `json:"billable_seconds"`
	RetryOf         *string `json:"retry_of"`
	CreatedAt       *int64  `json:"created_at"`
	Options         struct {
		LanguageCode         *string  `json:"language_code"`
		SpeakerDetection     bool     `json:"speaker_detection"`
		MedicalMode          bool     `json:"medical_mode"`
		Multichannel         bool     `json:"multichannel"`
		TranslationTargets   []string `json:"translation_targets"`
		Keyterms             bool     `json:"keyterms"`
		Prompt               bool     `json:"prompt"`
		AutoGenerateInsights *bool    `json:"auto_generate_insights"`
	} `json:"options"`
}

func valueOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (b jobBody) job() *Job {
	job := &Job{
		ID: b.ID, State: b.State, TranscriptID: valueOf(b.TranscriptID), Error: valueOf(b.Error), FailureKind: valueOf(b.FailureKind),
		Mode: b.Mode, DeclaredSeconds: b.DeclaredSeconds, VerifiedSeconds: b.VerifiedSeconds, ActualSeconds: b.ActualSeconds,
		BillableSeconds: b.BillableSeconds, RetryOf: valueOf(b.RetryOf),
		Options: JobOptions{
			LanguageCode: valueOf(b.Options.LanguageCode), SpeakerDetection: b.Options.SpeakerDetection, MedicalMode: b.Options.MedicalMode,
			Multichannel: b.Options.Multichannel, TranslationTargets: b.Options.TranslationTargets, Keyterms: b.Options.Keyterms,
			Prompt: b.Options.Prompt, AutoGenerateInsights: b.Options.AutoGenerateInsights,
		},
	}
	if b.CreatedAt != nil {
		job.CreatedAt = unixTime(*b.CreatedAt)
	}
	return job
}

// JobsService is the friendly layer over the jobs endpoints: Client.Jobs.
type JobsService struct {
	core *core
}

// JobListOptions are the options of Jobs.List.
type JobListOptions struct {
	// Active keeps only the jobs that have not finished.
	Active bool
}

// Get reads one job.
func (j JobsService) Get(ctx context.Context, id string) (*Job, error) {
	if j.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	job, err := j.core.readJob(ctx, id)
	if err != nil {
		return nil, (&annotator{core: j.core}).annotate(ctx, err)
	}
	return job, nil
}

// List walks every job, newest first, fetching pages as it goes; a nil opts lists them all. The
// iterator yields an error once and then ends. ctx is checked before every page and every job.
func (j JobsService) List(ctx context.Context, opts *JobListOptions) iter.Seq2[Job, error] {
	if j.core == nil {
		return failedSeq[Job](newConfigError(notBuiltMessage))
	}
	if opts == nil {
		opts = &JobListOptions{}
	}
	core := j.core
	return paginate(ctx, core, func(ctx context.Context, cursor string) (Page[Job], error) {
		const route = "GET /v1/jobs"
		params := &generated.ListJobsParams{}
		if opts.Active {
			active := generated.ListJobsParamsActive("true")
			params.Active = &active
		}
		if cursor != "" {
			params.Cursor = &cursor
		}
		response, err := core.api.ListJobsWithResponse(ctx, params)
		if err != nil {
			return Page[Job]{}, core.readFailed(err, route)
		}
		if failure := core.unusable(route, response.HTTPResponse, response.JSON200 != nil, response.Body, "jobs"); failure != nil {
			return Page[Job]{}, failure
		}
		var body struct {
			Jobs       []jobBody `json:"jobs"`
			NextCursor *string   `json:"next_cursor"`
		}
		if json.Unmarshal(response.Body, &body) != nil {
			return Page[Job]{}, core.badResponse(route, response.StatusCode())
		}
		page := Page[Job]{NextCursor: valueOf(body.NextCursor), Items: make([]Job, len(body.Jobs))}
		for i := range body.Jobs {
			page.Items[i] = *body.Jobs[i].job()
		}
		return page, nil
	})
}
