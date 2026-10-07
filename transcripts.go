package hushscript

import (
	"context"
	"encoding/json"
	"iter"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

// TranscriptSummary is one row of the transcript list.
type TranscriptSummary struct {
	ID    string
	Title string
	// Language is the detected language; "" when there is none.
	Language        string
	DurationSeconds int
	CreatedAt       time.Time
	// AutoDeleteAt is when the transcript is scheduled to be purged; nil keeps it indefinitely.
	AutoDeleteAt *time.Time
	Tags         []string
}

// DeletedTranscript is the record kept after a transcript was removed; its content is gone.
type DeletedTranscript struct {
	ID              string
	Title           string
	DurationSeconds int
	// CreatedAt is when the transcript was uploaded.
	CreatedAt time.Time
	DeletedAt time.Time
	// Cause is manual, scheduled, dormant or expired; a cause the SDK does not know yet passes through
	// as the API's own string.
	Cause string
	// Receipt is the stored deletion receipt, opaque and as the API sends it (snake_case keys, string
	// timestamps); nil when it could not be read back.
	Receipt map[string]any
}

// TranscriptsService is the friendly layer over the transcript endpoints: Client.Transcripts.
type TranscriptsService struct {
	core *core
}

// ListOptions are the options of Transcripts.List.
type ListOptions struct {
	// PageSize is the rows per page: 0 for the API's default of 50, otherwise 1 or more (the API clamps
	// it at 100).
	PageSize int
}

// PageOptions are the options of Transcripts.Page.
type PageOptions struct {
	// Cursor is a previous page's NextCursor; "" fetches the first page.
	Cursor string
	// PageSize is as in ListOptions.
	PageSize int
}

func (c *core) checkPageSize(size int) error {
	if size < 0 {
		return invalidOption(c.cfg.redactor(), "PageSize must be a whole number of 1 or more.", nil)
	}
	return nil
}

// List walks every finished transcript, newest first, fetching pages as it goes; a nil opts uses the
// default page size. The iterator yields an error once and then ends. ctx is checked before every page
// and every row. Transcripts still processing are jobs: see Jobs.List with Active.
func (t TranscriptsService) List(ctx context.Context, opts *ListOptions) iter.Seq2[TranscriptSummary, error] {
	if t.core == nil {
		return failedSeq[TranscriptSummary](newConfigError(notBuiltMessage))
	}
	if opts == nil {
		opts = &ListOptions{}
	}
	if err := t.core.checkPageSize(opts.PageSize); err != nil {
		return failedSeq[TranscriptSummary](err)
	}
	return paginate(ctx, t.core, func(ctx context.Context, cursor string) (Page[TranscriptSummary], error) {
		return t.core.transcriptPage(ctx, cursor, opts.PageSize)
	})
}

// Page fetches one page of List.
func (t TranscriptsService) Page(ctx context.Context, opts *PageOptions) (*Page[TranscriptSummary], error) {
	if t.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	if opts == nil {
		opts = &PageOptions{}
	}
	if err := t.core.checkPageSize(opts.PageSize); err != nil {
		return nil, err
	}
	page, err := t.core.transcriptPage(ctx, opts.Cursor, opts.PageSize)
	if err != nil {
		return nil, (&annotator{core: t.core}).annotate(ctx, err)
	}
	return &page, nil
}

func (c *core) transcriptPage(ctx context.Context, cursor string, size int) (Page[TranscriptSummary], error) {
	const route = "GET /v1/transcripts"
	params := &generated.ListTranscriptsParams{}
	if cursor != "" {
		params.Cursor = &cursor
	}
	if size > 0 {
		params.Limit = &size
	}
	response, err := c.api.ListTranscriptsWithResponse(ctx, params)
	if err != nil {
		return Page[TranscriptSummary]{}, c.readFailed(err, route)
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := c.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "transcripts"); failure != nil {
		return Page[TranscriptSummary]{}, failure
	}
	if !rowsHave(response.Body, "transcripts", "id", "created_at") {
		return Page[TranscriptSummary]{}, c.badResponse(route, response.StatusCode())
	}
	list := parsed
	page := Page[TranscriptSummary]{NextCursor: valueOf(list.NextCursor), Items: make([]TranscriptSummary, len(list.Transcripts))}
	for i := range list.Transcripts {
		row := &list.Transcripts[i]
		summary := TranscriptSummary{
			ID: row.Id, Title: row.Title, Language: valueOf(row.LanguageDetected), DurationSeconds: row.DurationSeconds,
			CreatedAt: unixTime(int64(row.CreatedAt)), Tags: row.Tags,
		}
		if summary.Tags == nil {
			summary.Tags = []string{}
		}
		if row.AutoDeleteAt != nil {
			at := unixTime(int64(*row.AutoDeleteAt))
			summary.AutoDeleteAt = &at
		}
		page.Items[i] = summary
	}
	return page, nil
}

// Get reads one transcript, with its formatters.
func (t TranscriptsService) Get(ctx context.Context, id string) (*Transcript, error) {
	if t.core == nil {
		return nil, newConfigError(notBuiltMessage)
	}
	if err := checkID(t.core.cfg.redactor(), "The transcript id", id); err != nil {
		return nil, err
	}
	transcript, err := t.core.readTranscript(ctx, id)
	if err != nil {
		return nil, (&annotator{core: t.core}).annotate(ctx, err)
	}
	return transcript, nil
}

// Deleted walks the deletion records, by upload time, newest first. The iterator is as in List.
func (t TranscriptsService) Deleted(ctx context.Context) iter.Seq2[DeletedTranscript, error] {
	if t.core == nil {
		return failedSeq[DeletedTranscript](newConfigError(notBuiltMessage))
	}
	core := t.core
	return paginate(ctx, core, func(ctx context.Context, cursor string) (Page[DeletedTranscript], error) {
		const route = "GET /v1/transcripts/deleted"
		params := &generated.ListDeletedTranscriptsParams{}
		if cursor != "" {
			params.Cursor = &cursor
		}
		response, err := core.api.ListDeletedTranscriptsWithResponse(ctx, params)
		if err != nil {
			return Page[DeletedTranscript]{}, core.readFailed(err, route)
		}
		parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
		if failure := core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "deleted"); failure != nil {
			return Page[DeletedTranscript]{}, failure
		}
		if !rowsHave(response.Body, "deleted", "id", "created_at", "deleted_at") {
			return Page[DeletedTranscript]{}, core.badResponse(route, response.StatusCode())
		}
		var body struct {
			Deleted []struct {
				ID              string         `json:"id"`
				Title           string         `json:"title"`
				DurationSeconds int            `json:"duration_seconds"`
				CreatedAt       int64          `json:"created_at"`
				DeletedAt       int64          `json:"deleted_at"`
				Cause           string         `json:"cause"`
				Receipt         map[string]any `json:"receipt"`
			} `json:"deleted"`
			NextCursor *string `json:"next_cursor"`
		}
		if json.Unmarshal(response.Body, &body) != nil {
			return Page[DeletedTranscript]{}, core.badResponse(route, response.StatusCode())
		}
		page := Page[DeletedTranscript]{NextCursor: valueOf(body.NextCursor), Items: make([]DeletedTranscript, len(body.Deleted))}
		for i, row := range body.Deleted {
			page.Items[i] = DeletedTranscript{
				ID: row.ID, Title: row.Title, DurationSeconds: row.DurationSeconds, CreatedAt: unixTime(row.CreatedAt),
				DeletedAt: unixTime(row.DeletedAt), Cause: row.Cause, Receipt: row.Receipt,
			}
		}
		return page, nil
	})
}
