package hushscript

import (
	"context"
	"iter"
)

// Page is one page of a list. NextCursor is opaque and "" on the last page.
type Page[T any] struct {
	Items      []T
	NextCursor string
}

// failedSeq is an iterator that yields err once.
func failedSeq[T any](err error) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		yield(zero, err)
	}
}

func (c *core) paginationLoop() *Error {
	return newSDKError(KindUnexpected, "pagination_loop",
		"The API returned the same page cursor it was sent; stopping instead of repeating the page.", nil, c.cfg.redactor(), hintInputs{})
}

// paginate walks a cursor-paged list one page at a time, yielding every item in order. ctx is checked
// before every page and every item, so a cancellation between pages or mid-page ends the walk with the
// SDK's abort error. A page that hands back the cursor it was asked with would repeat forever, so it
// is the pagination_loop error. The walk stops, with no further request, as soon as the caller does.
func paginate[T any](ctx context.Context, c *core, fetch func(ctx context.Context, cursor string) (Page[T], error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		ann := &annotator{core: c}
		cursor := ""
		for {
			if stopped := ctx.Err(); stopped != nil {
				yield(zero, c.abortError(stopped))
				return
			}
			page, err := fetch(ctx, cursor)
			if err != nil {
				yield(zero, ann.annotate(ctx, err))
				return
			}
			for _, item := range page.Items {
				if stopped := ctx.Err(); stopped != nil {
					yield(zero, c.abortError(stopped))
					return
				}
				if !yield(item, nil) {
					return
				}
			}
			if page.NextCursor == "" {
				return
			}
			if page.NextCursor == cursor {
				yield(zero, c.paginationLoop())
				return
			}
			cursor = page.NextCursor
		}
	}
}
