package hushscript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// virtualClock is a clock that only moves when told to: Sleep blocks until the time has been advanced
// past it. Tests either move it by hand or let run() move it whenever the SDK has nothing else to do.
type virtualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*virtualTimer
	slept  []time.Duration
}

type virtualTimer struct {
	at   time.Time
	done chan struct{}
}

func newVirtualClock() *virtualClock { return &virtualClock{now: time.Unix(1_000_000, 0)} }

func (c *virtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.slept = append(c.slept, d)
	if d <= 0 {
		c.mu.Unlock()
		return nil
	}
	timer := &virtualTimer{at: c.now.Add(d), done: make(chan struct{})}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	select {
	case <-timer.done:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		for i, existing := range c.timers {
			if existing == timer {
				c.timers = append(c.timers[:i], c.timers[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *virtualClock) sleepers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *virtualClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// earliest is the time of the first pending timer.
func (c *virtualClock) earliest() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var first time.Time
	for i, timer := range c.timers {
		if i == 0 || timer.at.Before(first) {
			first = timer.at
		}
	}
	return first, len(c.timers) > 0
}

// advanceTo moves time to at (never backwards) and releases every timer due by then.
func (c *virtualClock) advanceTo(at time.Time) {
	c.mu.Lock()
	if at.After(c.now) {
		c.now = at
	}
	var due []*virtualTimer
	rest := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.at.After(c.now) {
			due = append(due, timer)
		} else {
			rest = append(rest, timer)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	for _, timer := range due {
		close(timer.done)
	}
}

func (c *virtualClock) advance(d time.Duration) { c.advanceTo(c.Now().Add(d)) }

// run moves the clock to the next timer whenever the API has no request in flight and the SDK has
// been waiting on time for a few milliseconds. It stops when the test ends.
func (c *virtualClock) run(t *testing.T, api *fakeAPI) {
	t.Helper()
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		quiet := 0
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if api.inFlight.Load() == 0 && c.sleepers() > 0 {
				quiet++
			} else {
				quiet = 0
			}
			if quiet >= 8 {
				if at, ok := c.earliest(); ok {
					c.advanceTo(at)
				}
				quiet = 0
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-finished
	})
}

// seen is one request the fake API received.
type seen struct {
	route         string
	method        string
	path          string
	query         string
	authorization bool
	contentType   string
	contentLength int64
	body          []byte
	n             int
	at            time.Time
}

// fakeAPI is an in-process stand-in for the Hushscript API and its R2 part endpoint, on one server,
// so the part URLs have the API's own host and the SDK must keep the token off them by itself.
type fakeAPI struct {
	*httptest.Server
	t     *testing.T
	clock Clock

	mu   sync.Mutex
	log  []seen
	jobs map[string]string // idempotency key -> job id

	inFlight atomic.Int64

	// configuration, set before the first request
	partSize    int64
	proxiedOnly bool
	pageSize    int
	balance     int
	maxDaily    int
	usedToday   int
	completeAs  string
	jobStates   []string
	jobFields   map[string]any
	transcript  string
	hook        func(s *seen) (status int, body string, handled bool)
	onR2        func(n int)
	urlBase     string // where part URLs point; default this server
	deadPart    int    // a part whose URL points at a closed port

	parts      map[int]partRecord
	r2Failures []string
	heldR2     chan struct{}
	r2Active   atomic.Int64
	r2Peak     atomic.Int64
	size       int64
	jobReads   int
	completeOK atomic.Bool
}

type partRecord struct {
	etag string
	hash [32]byte
	size int64
}

func newFakeAPI(t *testing.T, clock Clock) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t: t, clock: clock, jobs: map[string]string{}, parts: map[int]partRecord{},
		partSize: 256_000, pageSize: 16, balance: 36000, maxDaily: 100000,
		completeAs: "queued", jobStates: []string{"processing", "done"},
		transcript: `{"id":"tr_1","title":"T","language_detected":"en","duration_seconds":20,"body":{"version":1,"original":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":1000,"text":"hi"}]},"translations":{}},"x_unknown_field":{"nested":true}}`,
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// setHook installs (or removes) the request hook while requests may be in flight.
func (f *fakeAPI) setHook(hook func(s *seen) (int, string, bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = hook
}

// set changes the configuration while requests may be in flight.
func (f *fakeAPI) set(change func(*fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeAPI) count(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.log {
		if f.log[i].route == route {
			n++
		}
	}
	return n
}

// totalExceptParts counts every request but the part uploads, which can still arrive from an
// earlier, cancelled run after its call has returned.
func (f *fakeAPI) totalExceptParts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for i := range f.log {
		if r := f.log[i].route; r != "r2" && r != "part" {
			n++
		}
	}
	return n
}

func (f *fakeAPI) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.log)
}

func (f *fakeAPI) requests(route string) []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []seen
	for i := range f.log {
		if f.log[i].route == route {
			out = append(out, f.log[i])
		}
	}
	return out
}

func routeOf(method, path string) (route string, n int) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	number := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case method == http.MethodGet && path == "/v1/account":
		return "account", 0
	case method == http.MethodPost && path == "/v1/uploads":
		return "create", 0
	case method == http.MethodGet && len(parts) == 4 && parts[1] == "uploads" && parts[3] == "part-urls":
		return "partUrls", 0
	case method == http.MethodPut && len(parts) == 3 && parts[0] == "r2":
		return "r2", number(parts[2])
	case method == http.MethodPut && len(parts) == 5 && parts[1] == "uploads" && parts[3] == "parts":
		return "part", number(parts[4])
	case method == http.MethodPost && len(parts) == 4 && parts[3] == "heartbeat":
		return "heartbeat", 0
	case method == http.MethodPost && len(parts) == 4 && parts[3] == "complete":
		return "complete", 0
	case method == http.MethodGet && len(parts) == 3 && parts[1] == "jobs":
		return "job", 0
	case method == http.MethodGet && len(parts) == 3 && parts[1] == "transcripts":
		return "transcript", 0
	}
	return "other", 0
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body) //nolint:gosec // a canned JSON body from the test, not user input
}

func apiError(w http.ResponseWriter, status int, slug string, code int, extra string) {
	writeJSON(w, status, fmt.Sprintf(`{"error":%q,"code":%d%s}`, slug, code, extra))
}

// totalParts is how many parts the job has; a proxied job is cut at the API's own 32 MiB bound.
func (f *fakeAPI) totalParts(size int64) int {
	partSize := f.partSize
	if f.proxiedOnly {
		partSize = 32 << 20
	}
	return int((size + partSize - 1) / partSize)
}

func (f *fakeAPI) partURLs(job string, from, total int) (urls []map[string]any, next any) {
	end := min(from+f.pageSize-1, total)
	for n := from; n <= end; n++ {
		base := f.URL
		if f.urlBase != "" {
			base = f.urlBase
		}
		if n == f.deadPart {
			base = "http://127.0.0.1:1"
		}
		urls = append(urls, map[string]any{"n": n, "url": fmt.Sprintf("%s/r2/%s/%d?X-Amz-Signature=sig%d", base, job, n, n)})
	}
	if end < total {
		next = end + 1
	}
	return urls, next
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	route, n := routeOf(r.Method, r.URL.Path)
	rec := seen{
		route: route, method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, n: n,
		authorization: r.Header.Get("Authorization") != "", contentType: r.Header.Get("Content-Type"),
		contentLength: r.ContentLength, at: f.clock.Now(),
	}
	hasher := sha256.New()
	var read int64
	if route == "r2" || route == "part" {
		read, _ = io.Copy(hasher, r.Body)
	} else {
		rec.body, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.log = append(f.log, rec)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if status, body, handled := hook(&rec); handled {
			if status == 0 { // the hook asks for a dropped connection
				if hijacker, ok := w.(http.Hijacker); ok {
					if conn, _, err := hijacker.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			writeJSON(w, status, body)
			return
		}
	}
	switch route {
	case "account":
		f.mu.Lock()
		balance, maxDaily, usedToday := f.balance, f.maxDaily, f.usedToday
		f.mu.Unlock()
		writeJSON(w, 200, fmt.Sprintf(`{"balance_seconds":%d,"held_seconds":0,"card_on_file":true,"data_region":"eu","credits_expire_at":null,"limits":{"active_jobs":0,"max_active_jobs":50,"max_daily_seconds":%d,"used_today_seconds":%d}}`, balance, maxDaily, usedToday))
	case "create":
		f.create(w, rec)
	case "partUrls":
		job := strings.Split(r.URL.Path, "/")[3]
		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		start = max(start, 1)
		f.mu.Lock()
		total := f.totalParts(f.size)
		f.mu.Unlock()
		urls, next := f.partURLs(job, start, total)
		out, _ := json.Marshal(map[string]any{"part_size_bytes": f.partSize, "total_parts": total, "part_urls": urls, "next_part_number": next})
		writeJSON(w, 200, string(out))
	case "r2", "part":
		f.storePart(w, r, route, n, hasher.Sum(nil), read)
	case "heartbeat":
		writeJSON(w, 200, `{"ok":true}`)
	case "complete":
		f.complete(w, rec)
	case "job":
		f.job(w, r)
	case "transcript":
		writeJSON(w, 200, f.transcript)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) create(w http.ResponseWriter, rec seen) {
	var body struct {
		Size int64  `json:"size_bytes"`
		Key  string `json:"idempotency_key"`
	}
	_ = json.Unmarshal(rec.body, &body)
	f.mu.Lock()
	job, replay := f.jobs[body.Key]
	if !replay {
		job = fmt.Sprintf("job_%d", len(f.jobs)+1)
		f.jobs[body.Key] = job
		f.size = body.Size
	}
	total := f.totalParts(f.size)
	f.mu.Unlock()
	state := "uploading"
	var upload any
	if !f.proxiedOnly {
		urls, next := f.partURLs(job, 1, total)
		upload = map[string]any{"part_size_bytes": f.partSize, "total_parts": total, "part_urls": urls, "next_part_number": next}
	}
	if replay && f.completeOK.Load() {
		state, upload = "queued", nil
	}
	out, _ := json.Marshal(map[string]any{"job_id": job, "state": state, "balance_seconds": 1, "held_seconds": 1, "funding": "personal", "limits": map[string]int{"active_jobs": 1, "max_active_jobs": 50, "max_daily_seconds": 1, "used_today_seconds": 0}, "upload": upload})
	writeJSON(w, 200, string(out))
}

func (f *fakeAPI) storePart(w http.ResponseWriter, r *http.Request, route string, n int, sum []byte, read int64) {
	if route == "r2" {
		active := f.r2Active.Add(1)
		defer f.r2Active.Add(-1)
		for {
			peak := f.r2Peak.Load()
			if active <= peak || f.r2Peak.CompareAndSwap(peak, active) {
				break
			}
		}
		if f.onR2 != nil {
			f.onR2(n)
		}
		if f.heldR2 != nil {
			select {
			case <-f.heldR2:
			case <-r.Context().Done():
				return
			}
		}
		problems := []string{}
		if r.Header.Get("Authorization") != "" {
			problems = append(problems, fmt.Sprintf("part %d carried Authorization", n))
		}
		if r.ContentLength != read {
			problems = append(problems, fmt.Sprintf("part %d: Content-Length %d, body %d", n, r.ContentLength, read))
		}
		if !strings.HasPrefix(r.URL.RawQuery, "X-Amz-Signature=sig") {
			problems = append(problems, fmt.Sprintf("part %d lost its signature", n))
		}
		if len(problems) > 0 {
			f.mu.Lock()
			f.r2Failures = append(f.r2Failures, problems...)
			f.mu.Unlock()
		}
	}
	record := partRecord{etag: fmt.Sprintf("etag-%d", n), size: read}
	copy(record.hash[:], sum)
	f.mu.Lock()
	f.parts[n] = record
	f.mu.Unlock()
	if route == "r2" {
		w.Header().Set("ETag", `"`+record.etag+`"`)
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, 200, fmt.Sprintf(`{"ok":true,"part":%d}`, n))
}

func (f *fakeAPI) complete(w http.ResponseWriter, rec seen) {
	var body struct {
		Parts []struct {
			N    int    `json:"n"`
			Etag string `json:"etag"`
		} `json:"parts"`
	}
	if len(rec.body) > 0 {
		_ = json.Unmarshal(rec.body, &body)
	}
	f.mu.Lock()
	total := f.totalParts(f.size)
	complete := len(f.parts) == total
	f.mu.Unlock()
	if len(body.Parts) > 0 {
		sorted := sort.SliceIsSorted(body.Parts, func(i, j int) bool { return body.Parts[i].N < body.Parts[j].N })
		complete = complete && sorted && len(body.Parts) == total
		for _, p := range body.Parts {
			if p.Etag != fmt.Sprintf("etag-%d", p.N) {
				complete = false
			}
		}
	}
	if !complete {
		apiError(w, 422, "upload_incomplete", 1041, "")
		return
	}
	f.completeOK.Store(true)
	writeJSON(w, 200, fmt.Sprintf(`{"ok":true,"state":%q,"balance_seconds":1,"held_seconds":0,"verified_seconds":20}`, f.completeAs))
}

func (f *fakeAPI) job(w http.ResponseWriter, r *http.Request) {
	id := strings.Split(r.URL.Path, "/")[3]
	f.mu.Lock()
	state := f.jobStates[min(f.jobReads, len(f.jobStates)-1)]
	f.jobReads++
	fields := f.jobFields
	f.mu.Unlock()
	job := map[string]any{
		"id": id, "state": state, "mode": "standard", "declared_seconds": 20, "billable_seconds": 20, "created_at": 1,
		"error": nil, "failure_kind": nil, "options": map[string]any{}, "private_result_available": false, "private_result_expires_at": nil,
		"retry_of": nil, "transcript_id": nil, "verified_seconds": nil, "actual_seconds": nil,
	}
	if state == "done" {
		job["transcript_id"] = "tr_1"
	}
	for k, v := range fields {
		job[k] = v
	}
	out, _ := json.Marshal(job)
	writeJSON(w, 200, string(out))
}

// testWAV is mono 16-bit audio at 16 kHz (32000 bytes a second) filled with seeded noise, so every
// part of the file has its own bytes.
func testWAV(seconds int) []byte {
	data := make([]byte, seconds*32000)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // seeded noise for a fixture, not a secret
	for i := range data {
		data[i] = byte(rng.UintN(256)) //nolint:gosec // UintN(256) fits a byte
	}
	return append(wavHeader(len(data)), data...)
}

func wavHeader(dataSize int) []byte {
	var h bytes.Buffer
	h.WriteString("RIFF")
	_ = binary.Write(&h, binary.LittleEndian, uint32(36+dataSize)) //nolint:gosec // test sizes are small
	h.WriteString("WAVEfmt ")
	_ = binary.Write(&h, binary.LittleEndian, uint32(16))
	_ = binary.Write(&h, binary.LittleEndian, uint16(1))
	_ = binary.Write(&h, binary.LittleEndian, uint16(1))
	_ = binary.Write(&h, binary.LittleEndian, uint32(16000))
	_ = binary.Write(&h, binary.LittleEndian, uint32(32000))
	_ = binary.Write(&h, binary.LittleEndian, uint16(2))
	_ = binary.Write(&h, binary.LittleEndian, uint16(16))
	h.WriteString("data")
	_ = binary.Write(&h, binary.LittleEndian, uint32(dataSize)) //nolint:gosec // test sizes are small
	return h.Bytes()
}

// partHashes are the SHA-256 of each part of data cut at partSize.
func partHashes(data []byte, partSize int64) map[int][32]byte {
	out := map[int][32]byte{}
	for n, off := 1, int64(0); off < int64(len(data)); n, off = n+1, off+partSize {
		out[n] = sha256.Sum256(data[off:min(off+partSize, int64(len(data)))])
	}
	return out
}

// pipelineClient builds a client against the fake API on the given clock.
func pipelineClient(t *testing.T, api *fakeAPI, clock Clock, opts ...Option) *Client {
	t.Helper()
	return newTestClient(t, api.URL, clock, opts...)
}
