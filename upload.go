package hushscript

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Hushscript/hushscript-go/generated"
)

const (
	// proxiedPartBytes is the API's upper bound for a part sent through it (spec 8.4).
	proxiedPartBytes = 32 << 20
	// urlMaxAge: presigned URLs live 24 h, so a page older than this is fetched again.
	urlMaxAge = 23 * time.Hour
)

// uploadPlan is the direct-to-R2 plan of a create response; nil stands for a proxied job.
type uploadPlan struct {
	partSize   int64
	totalParts int
	urls       []partURL
	next       int // 0 once every part has a URL
}

// partURL is a presigned part URL, held as a secret because its query is a signature.
type partURL struct {
	n   int
	url secret
}

func partURLsOf(rows []generated.PresignedPartUrl) []partURL {
	urls := make([]partURL, len(rows))
	for i, row := range rows {
		urls[i] = partURL{n: row.N, url: newSecret(row.Url)}
	}
	return urls
}

// nullPartURLs reports whether body is an object whose part_urls is a JSON null: the same "direct
// upload is not available" answer as a null page.
func nullPartURLs(body []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return false
	}
	value, ok := root["part_urls"]
	return ok && string(bytes.TrimSpace(value)) == "null"
}

// partRowsUsable reports whether every part URL row has a positive part number and a URL.
func partRowsUsable(rows []generated.PresignedPartUrl) bool {
	for _, row := range rows {
		if row.N <= 0 || row.Url == "" {
			return false
		}
	}
	return true
}

// planOf reads the direct-upload plan of a create answer; nil stands for a proxied job. A part size
// or part count that is not a positive whole number cannot be a plan at all: that is a bad_response
// with the status the API answered with, not a bad_part_plan, which is for a plan that disagrees with
// itself (the part count is not the file's size over the part size, checked by newUploader).
func (c *core) planOf(created *generated.UploadCreateResponse, status int) (*uploadPlan, *Error) {
	if created.Upload == nil {
		return nil, nil
	}
	if created.Upload.PartSizeBytes < 1 || created.Upload.TotalParts < 1 {
		return nil, c.badResponse("POST /v1/uploads", status)
	}
	plan := &uploadPlan{
		partSize:   int64(created.Upload.PartSizeBytes),
		totalParts: created.Upload.TotalParts,
		urls:       partURLsOf(created.Upload.PartUrls),
	}
	if created.Upload.NextPartNumber != nil {
		plan.next = *created.Upload.NextPartNumber
	}
	return plan, nil
}

type completedPart struct {
	N    int    `json:"n"`
	Etag string `json:"etag"`
}

// upload sends every part, keeps the job alive meanwhile and completes it. It returns the job state
// the complete call reported, which is authoritative (spec 8.5).
func (r *run) upload(parent context.Context, plan *uploadPlan) (string, error) {
	c := r.core
	ctx, cancel := context.WithCancel(parent)
	beat := c.startHeartbeat(ctx, cancel, r.state.JobID)
	state, err := r.sendAndComplete(ctx, plan)
	cancel()
	failure := beat.stop()
	if raised, isPanic := err.(*callbackPanic); isPanic { //nolint:errorlint // sendAndComplete returns it unwrapped
		panic(raised.value) // the caller's own callback panicked on an upload worker: its value, unchanged, on the caller's goroutine
	}
	if failure != nil {
		// A refused token ended the upload; whatever the cancellation then broke is a symptom.
		return "", failure
	}
	return state, err
}

func (r *run) sendAndComplete(ctx context.Context, plan *uploadPlan) (string, error) {
	c := r.core
	up, err := c.newUploader(r, plan)
	if err != nil {
		return "", err
	}
	acks, err := up.send(ctx)
	if err != nil {
		return "", err
	}
	var body io.Reader
	var editors []generated.RequestEditorFn
	if acks != nil {
		encoded, marshalErr := json.Marshal(struct {
			Parts []completedPart `json:"parts"`
		}{acks})
		if marshalErr != nil {
			return "", c.requestNotBuilt(marshalErr)
		}
		body = bytes.NewReader(encoded)
	} else {
		// A proxied job completes with no body at all, so there is no content type either.
		editors = append(editors, func(_ context.Context, req *http.Request) error {
			req.Header.Del("Content-Type")
			return nil
		})
	}
	const route = "POST /v1/uploads/{job}/complete"
	response, err := c.api.CompleteUploadWithBodyWithResponse(ctx, r.state.JobID, "application/json", body, editors...)
	if err != nil {
		return "", c.readFailed(err, route)
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := c.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "state"); failure != nil {
		return "", failure
	}
	return parsed.State, nil
}

const (
	modeUndecided uint32 = iota
	modeDirect
	modeProxied
)

// uploader sends the parts of one job with at most `workers` in flight.
type uploader struct {
	core     *core
	red      redactor
	job      string
	src      *fileSource
	partSize int64
	total    int
	workers  int
	report   func(int64)

	pages *partPages // nil for a proxied job
	// mode is how the job's parts travel. It starts undecided and is settled once, by a
	// compare-and-swap: to modeDirect by the first direct PUT that succeeds, to modeProxied by the
	// first fallback. Whichever wins, the other can never happen, so the complete call is built
	// from exactly the parts that went the settled way.
	mode     atomic.Uint32
	nextPart atomic.Int64

	mu    sync.Mutex
	etags map[int]string

	// stop cancels the upload's workers; send sets it before any worker starts.
	stop context.CancelFunc
	// panicked holds the first panic a caller's OnProgress raised on a worker, for upload to re-raise.
	panicked atomic.Pointer[callbackPanic]
}

// callbackPanic carries the value a caller's callback panicked with from an upload worker, where
// nobody can recover it, back to the goroutine that made the call. The SDK never recovers anything
// else, and never wraps the value.
type callbackPanic struct{ value any }

func (*callbackPanic) Error() string { return "a callback panicked" }

// progress runs the caller's OnProgress under the lock that keeps its reports in order. A panic in it
// stops the upload and is kept for upload to raise again; later reports are not delivered.
func (u *uploader) progress(onProgress func(Progress), report Progress) {
	defer func() {
		if value := recover(); value != nil {
			u.panicked.CompareAndSwap(nil, &callbackPanic{value: value})
			u.stop()
		}
	}()
	if u.panicked.Load() == nil {
		onProgress(report)
	}
}

func (c *core) newUploader(r *run, plan *uploadPlan) (*uploader, error) {
	red := c.cfg.redactor()
	partSize := int64(proxiedPartBytes)
	if plan != nil {
		partSize = plan.partSize
	}
	total := int((r.src.size + partSize - 1) / partSize)
	if plan != nil && plan.totalParts != total {
		return nil, newSDKError(KindUpload, "bad_part_plan",
			"The API planned "+strconv.Itoa(plan.totalParts)+" parts; the file needs "+strconv.Itoa(total)+".", nil, red, hintInputs{})
	}
	u := &uploader{
		core: c, red: red, job: r.state.JobID, src: r.src, partSize: partSize, total: total,
		workers: min(r.concurrency, total), etags: map[int]string{},
	}
	if plan != nil {
		u.pages = newPartPages(c, u.job, plan)
	} else {
		u.mode.Store(modeProxied)
	}
	var sentMu sync.Mutex
	var sent int64
	u.report = func(n int64) {
		sentMu.Lock()
		defer sentMu.Unlock()
		sent += n
		if r.onProgress != nil {
			u.progress(r.onProgress, Progress{Stage: StageUploading, Percent: int(sent * 100 / r.src.size), BytesSent: sent, BytesTotal: r.src.size})
		}
	}
	return u, nil
}

// send uploads every part and returns the ETags to complete with, or nil when the job went through the
// API, whose complete call takes no body. Memory is bounded by workers times the part size: each
// worker holds one buffer and reuses it for every part it sends.
func (u *uploader) send(parent context.Context) ([]completedPart, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	u.stop = cancel
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for range u.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := u.worker(ctx); err != nil {
				once.Do(func() {
					first = err
					cancel()
				})
			}
		}()
	}
	wg.Wait()
	if raised := u.panicked.Load(); raised != nil {
		return nil, raised
	}
	if first != nil {
		return nil, first
	}
	if err := parent.Err(); err != nil {
		return nil, u.core.abortError(err)
	}
	if err := u.src.checkNoMoreBytes(u.red); err != nil {
		return nil, err
	}
	if u.isProxied() {
		return nil, nil
	}
	acks := make([]completedPart, 0, len(u.etags))
	for n, etag := range u.etags {
		acks = append(acks, completedPart{N: n, Etag: etag})
	}
	slices.SortFunc(acks, func(a, b completedPart) int { return a.N - b.N })
	return acks, nil
}

func (u *uploader) worker(ctx context.Context) error {
	var buf []byte
	for {
		if err := ctx.Err(); err != nil {
			return u.core.abortError(err)
		}
		n := int(u.nextPart.Add(1))
		if n > u.total {
			return nil
		}
		offset := int64(n-1) * u.partSize
		length := min(u.partSize, u.src.size-offset)
		if buf == nil {
			buf = make([]byte, min(u.partSize, u.src.size))
		}
		data := buf[:length]
		if err := u.src.readPart(data, offset, u.red); err != nil {
			return err
		}
		hold := &bodyHold{}
		if err := u.sendPart(ctx, n, data, hold); err != nil {
			return err
		}
		if !hold.wait(ctx) {
			buf = nil // cancelled while the transport still holds it: never write into it again
		}
		u.report(length)
	}
}

// bodyHold tracks the request bodies made from one part buffer, so the buffer is rewritten only after
// the transport has closed every one of them (a transport may still be reading after it answered).
type bodyHold struct {
	mu      sync.Mutex
	open    int
	changed chan struct{} // closed on the next release; nil while nobody waits
}

func (h *bodyHold) acquire() {
	h.mu.Lock()
	h.open++
	h.mu.Unlock()
}

func (h *bodyHold) release() {
	h.mu.Lock()
	h.open--
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
	h.mu.Unlock()
}

// wait blocks until every body has been closed, however long the transport takes, and reports true.
// It reports false only when ctx ends first.
func (h *bodyHold) wait(ctx context.Context) bool {
	for {
		h.mu.Lock()
		if h.open == 0 {
			h.mu.Unlock()
			return true
		}
		if h.changed == nil {
			h.changed = make(chan struct{})
		}
		changed := h.changed
		h.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false
		}
	}
}

type heldBody struct {
	*bytes.Reader
	once sync.Once
	hold *bodyHold
}

func (b *heldBody) Close() error {
	b.once.Do(b.hold.release)
	return nil
}

// partRequest is a PUT of data with its exact Content-Length, replayable for a retry without copying
// the bytes.
func partRequest(ctx context.Context, target string, data []byte, hold *bodyHold) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(data))
	req.GetBody = func() (io.ReadCloser, error) {
		hold.acquire()
		return &heldBody{Reader: bytes.NewReader(data), hold: hold}, nil
	}
	req.Body = http.NoBody
	return req, nil
}

// sendPart sends one part: direct first, with the fallback rules of spec 8.4, then through the API
// when the job went proxied.
func (u *uploader) sendPart(ctx context.Context, n int, data []byte, hold *bodyHold) error {
	if !u.isProxied() {
		if err := u.sendDirect(ctx, n, data, hold); err != nil {
			return err
		}
	}
	if u.isProxied() {
		return u.putProxied(ctx, n, data, hold)
	}
	return nil
}

type directState struct{ refetched bool }

func (u *uploader) sendDirect(ctx context.Context, n int, data []byte, hold *bodyHold) error {
	state := &directState{}
	for !u.isProxied() && u.pages != nil {
		target, direct, err := u.pages.urlFor(ctx, n)
		if err != nil {
			if recovered := u.recover(ctx, err, n, "", state); recovered != nil {
				return recovered
			}
			continue
		}
		if !direct {
			if !u.fallBack(n) {
				return u.partURLMissing(n)
			}
			return nil
		}
		etag, err := u.putDirect(ctx, n, target, data, hold)
		if err == nil {
			u.trace("direct-success", n)
			if !u.settleDirect() {
				return nil // the job went proxied first: sendPart sends this part through the API again
			}
			u.mu.Lock()
			u.etags[n] = etag
			u.mu.Unlock()
			u.trace("direct-done", n)
			return nil
		}
		if recovered := u.recover(ctx, err, n, target, state); recovered != nil {
			return recovered
		}
	}
	return nil
}

func (u *uploader) isProxied() bool { return u.mode.Load() == modeProxied }

func (u *uploader) isDirect() bool { return u.mode.Load() == modeDirect }

// settleDirect records a successful direct PUT. It reports false when the job already went proxied.
func (u *uploader) settleDirect() bool {
	return u.mode.CompareAndSwap(modeUndecided, modeDirect) || u.isDirect()
}

// fallBack moves an undecided job to proxied parts and reports true; it reports false when a direct
// PUT has already succeeded, which rules the fallback out (spec 8.4). It is true as well when
// another part already did the move.
func (u *uploader) fallBack(n int) bool {
	u.trace("fallback", n)
	return u.mode.CompareAndSwap(modeUndecided, modeProxied) || u.isProxied()
}

func (u *uploader) trace(event string, n int) {
	if u.core.uploadTrace != nil {
		u.core.uploadTrace(event, n)
	}
}

func (u *uploader) partURLMissing(n int) *Error {
	return newSDKError(KindUpload, "part_url_missing", "No upload URL for part "+strconv.Itoa(n)+".", nil, u.red, hintInputs{})
}

// isTerminal is a 4xx other than 429 (which was already retried): the API said no, and trying again
// will not change it.
func isTerminal(failure *Error) bool {
	return failure.Status >= http.StatusBadRequest && failure.Status < http.StatusInternalServerError &&
		failure.Status != http.StatusTooManyRequests
}

// recover decides what a failed direct attempt (the PUT, or the part-urls fetch behind it when
// target is "") leads to. A 403, or any failure once a direct PUT has succeeded, gets one fresh URL
// page; after that the job falls back to proxied parts if no direct PUT ever succeeded, and raises
// parts_exhausted if one did. A part that lost the swap to a direct success was owed that fresh page
// as well, so it gets it before parts_exhausted. recover returns nil when the part should be tried
// again.
func (u *uploader) recover(ctx context.Context, err error, n int, target string, state *directState) error {
	failure, ok := err.(*Error) //nolint:errorlint // the doer and the pipeline return their own errors unwrapped
	if !ok || failure.Kind == KindAbort || (failure.Kind == KindUpload && failure.Code == "part_url_missing") || ctx.Err() != nil {
		return err
	}
	// The part-urls fetch itself was refused for a reason a fresh page or proxied parts cannot fix.
	if target == "" && u.isDirect() && isTerminal(failure) {
		return err
	}
	cause := failure
	if target != "" && !state.refetched && (failure.Status == http.StatusForbidden || u.isDirect()) {
		retry, refreshCause, fatal := u.refetch(ctx, n, target, state)
		if fatal != nil {
			return fatal
		}
		if retry {
			return nil
		}
		cause = refreshCause
	}
	if u.fallBack(n) {
		return nil
	}
	// The swap was lost to a direct success that came after the check above.
	if target != "" && !state.refetched {
		retry, refreshCause, fatal := u.refetch(ctx, n, target, state)
		if fatal != nil {
			return fatal
		}
		if retry {
			return nil
		}
		cause = refreshCause
	}
	return newSDKError(KindUpload, "parts_exhausted", "Part "+strconv.Itoa(n)+" failed after retries and a fresh URL.", cause, u.red, hintInputs{})
}

// refetch takes the one fresh URL page a failed direct PUT is owed. It reports retry when the part
// should be sent again, and a non-nil fatal when the fetch failed for a reason that ends the upload;
// otherwise cause is the fetch's own failure, for the error that follows.
func (u *uploader) refetch(ctx context.Context, n int, target string, state *directState) (retry bool, cause *Error, fatal error) {
	state.refetched = true
	refreshErr := u.pages.refresh(ctx, n, target)
	if refreshErr == nil {
		return true, nil, nil
	}
	refresh, isError := refreshErr.(*Error) //nolint:errorlint // the pipeline returns its own errors unwrapped
	if !isError || refresh.Kind == KindAbort || ctx.Err() != nil || (u.isDirect() && isTerminal(refresh)) {
		return false, nil, refreshErr
	}
	return false, refresh, nil
}

func (u *uploader) putDirect(ctx context.Context, n int, target string, data []byte, hold *bodyHold) (string, error) {
	req, err := partRequest(withDirectStorage(ctx, n), target, data, hold)
	if err != nil {
		return "", newSDKError(KindUpload, "bad_part_plan", "The API returned a part URL that cannot be used.", nil, u.red, hintInputs{})
	}
	resp, err := u.core.doer.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	if etag == "" {
		return "", newSDKError(KindUpload, "etag_missing", "Storage answered a part without an ETag.", nil, u.red, hintInputs{})
	}
	return etag, nil
}

func (u *uploader) putProxied(ctx context.Context, n int, data []byte, hold *bodyHold) error {
	target := u.core.cfg.baseURL + "/v1/uploads/" + url.PathEscape(u.job) + "/parts/" + strconv.Itoa(n)
	req, err := partRequest(withAttemptTimeout(ctx, partTimeout), target, data, hold)
	if err != nil {
		return u.core.requestNotBuilt(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := u.core.doer.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// partPages holds the presigned part URLs, 16 to a page, fetched on demand and again when expired.
type partPages struct {
	core *core
	job  string

	mu       sync.Mutex
	pages    []*urlPage
	inflight map[int]*pageFetch
	next     int // the first part number no page covers yet; 0 when every part has a URL
	direct   bool
}

type urlPage struct {
	start     int
	urls      map[int]secret
	fetchedAt time.Time
}

type pageFetch struct {
	done chan struct{}
	err  error
}

func newPartPages(c *core, job string, plan *uploadPlan) *partPages {
	p := &partPages{core: c, job: job, inflight: map[int]*pageFetch{}, next: plan.next, direct: true}
	if len(plan.urls) > 0 {
		p.store(plan.urls)
	}
	return p
}

// store records a page and returns its first part number. Call it without the lock held.
func (p *partPages) store(urls []partURL) int {
	start := urls[0].n
	page := &urlPage{urls: make(map[int]secret, len(urls)), fetchedAt: p.core.cfg.clock.Now()}
	for _, entry := range urls {
		start = min(start, entry.n)
		page.urls[entry.n] = entry.url
	}
	page.start = start
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.pages {
		if existing.start == start {
			p.pages[i] = page
			return start
		}
	}
	p.pages = append(p.pages, page)
	return start
}

func (p *partPages) pageOf(n int) *urlPage {
	for _, page := range p.pages {
		if _, ok := page.urls[n]; ok {
			return page
		}
	}
	return nil
}

// fetchPage makes one request per page start, however many parts wait for it.
func (p *partPages) fetchPage(ctx context.Context, start int) error {
	p.mu.Lock()
	if running, ok := p.inflight[start]; ok {
		p.mu.Unlock()
		select {
		case <-running.done:
			return running.err
		case <-ctx.Done():
			return p.core.abortError(ctx.Err())
		}
	}
	fetch := &pageFetch{done: make(chan struct{})}
	p.inflight[start] = fetch
	p.mu.Unlock()

	fetch.err = p.load(ctx, start)

	p.mu.Lock()
	delete(p.inflight, start)
	p.mu.Unlock()
	close(fetch.done)
	return fetch.err
}

func (p *partPages) load(ctx context.Context, start int) error {
	const route = "GET /v1/uploads/{job}/part-urls"
	response, err := p.core.api.GetUploadPartUrlsWithResponse(ctx, p.job, &generated.GetUploadPartUrlsParams{Start: &start})
	if err != nil {
		return p.core.readFailed(err, route)
	}
	if bytes.Equal(bytes.TrimSpace(response.Body), []byte("null")) && response.StatusCode() >= http.StatusOK && response.StatusCode() < http.StatusMultipleChoices {
		// A JSON null page says direct upload is not available for this job: its parts go through the API.
		p.mu.Lock()
		p.direct = false
		p.mu.Unlock()
		return nil
	}
	if nullPartURLs(response.Body) && response.StatusCode() >= http.StatusOK && response.StatusCode() < http.StatusMultipleChoices {
		p.mu.Lock()
		p.direct = false
		p.mu.Unlock()
		return nil
	}
	parsed := decodedBody(response.JSON200, response.HTTPResponse, response.Body)
	if failure := p.core.unusable(route, response.HTTPResponse, parsed != nil, response.Body, "part_urls"); failure != nil {
		return failure
	}
	if !rowsHave(response.Body, "part_urls", "n", "url") || !partRowsUsable(parsed.PartUrls) {
		return p.core.badResponse(route, response.StatusCode())
	}
	if len(parsed.PartUrls) == 0 {
		p.mu.Lock()
		p.direct = false
		p.mu.Unlock()
		return nil
	}
	stored := p.store(partURLsOf(parsed.PartUrls))
	p.mu.Lock()
	if p.next == stored {
		p.next = 0
		if parsed.NextPartNumber != nil {
			p.next = *parsed.NextPartNumber
		}
	}
	p.mu.Unlock()
	return nil
}

// maxPageFetches bounds the fetches one part may wait for, so an API that never lists a part ends in
// an error and not a loop.
const maxPageFetches = 4

// urlFor is the URL of part n; direct is false when the job turns out to take proxied parts only.
func (p *partPages) urlFor(ctx context.Context, n int) (signedURL string, direct bool, err error) {
	for range maxPageFetches {
		p.mu.Lock()
		hasDirect, page, next := p.direct, p.pageOf(n), p.next
		p.mu.Unlock()
		if !hasDirect {
			return "", false, nil
		}
		var start int
		switch {
		case page == nil && next == 0:
			return "", false, newSDKError(KindUpload, "part_url_missing", "No upload URL for part "+strconv.Itoa(n)+".", nil, p.core.cfg.redactor(), hintInputs{})
		case page == nil:
			start = next
		case p.core.cfg.clock.Now().Sub(page.fetchedAt) > urlMaxAge:
			start = page.start
		default:
			return page.urls[n].reveal(), true, nil
		}
		if fetchErr := p.fetchPage(ctx, start); fetchErr != nil {
			return "", false, fetchErr
		}
	}
	return "", false, newSDKError(KindUpload, "part_url_missing", "No upload URL for part "+strconv.Itoa(n)+".", nil, p.core.cfg.redactor(), hintInputs{})
}

// refresh fetches again the page that holds stale, unless another part already did.
func (p *partPages) refresh(ctx context.Context, n int, stale string) error {
	p.mu.Lock()
	page := p.pageOf(n)
	if page == nil || page.urls[n].reveal() != stale {
		p.mu.Unlock()
		return nil
	}
	start := page.start
	p.mu.Unlock()
	return p.fetchPage(ctx, start)
}
