package hushscript

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// listAPI answers GET requests by path with a canned handler and records what it was asked.
type listAPI struct {
	*httptest.Server
	mu      sync.Mutex
	queries []url.Values
	paths   []string
}

func newListAPI(t *testing.T, handler func(path string, query url.Values) (int, string)) *listAPI {
	t.Helper()
	api := &listAPI{}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		api.queries = append(api.queries, r.URL.Query())
		api.paths = append(api.paths, r.URL.Path)
		api.mu.Unlock()
		status, body := handler(r.URL.Path, r.URL.Query())
		writeJSON(w, status, body)
	}))
	t.Cleanup(api.Close)
	return api
}

func (a *listAPI) requests() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queries)
}

func (a *listAPI) query(i int) url.Values {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queries[i]
}

func (a *listAPI) client(t *testing.T) *Client {
	t.Helper()
	return newTestClient(t, a.URL, newFakeClock())
}

// rowsPage is a page of synthetic transcript rows from..to, linking to next.
func rowsPage(from, to int, next string) string {
	rows := make([]string, 0, to-from+1)
	for i := from; i <= to; i++ {
		rows = append(rows, fmt.Sprintf(`{"id":"tr_%03d","title":"T%d","language_detected":null,"duration_seconds":%d,"created_at":%d,"auto_delete_at":null,`+
			`"tags":["a"],"insights_state":"none","options":{},"x_unknown":1}`, i, i, i, 1760000000+i))
	}
	cursor := "null"
	if next != "" {
		cursor = strconv.Quote(next)
	}
	return fmt.Sprintf(`{"transcripts":[%s],"next_cursor":%s}`, strings.Join(rows, ","), cursor)
}

// threePages serves 7 rows as pages of 3, 3 and 1.
func threePages(_ string, query url.Values) (status int, body string) {
	switch query.Get("cursor") {
	case "":
		return 200, rowsPage(1, 3, "c1")
	case "c1":
		return 200, rowsPage(4, 6, "c2")
	case "c2":
		return 200, rowsPage(7, 7, "")
	}
	return 400, jsonError("bad_request", 4000)
}

func drain[T any](seq func(func(T, error) bool)) (items []T, err error) {
	for item, e := range seq {
		if e != nil {
			return items, e
		}
		items = append(items, item)
	}
	return items, nil
}

func TestTranscriptsListWalksEveryPageInOrderWithNoQueryOnTheFirst(t *testing.T) {
	api := newListAPI(t, threePages)
	items, err := drain(api.client(t).Transcripts.List(context.Background(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 7 || items[0].ID != "tr_001" || items[6].ID != "tr_007" {
		t.Fatalf("got %d items, first %v, last %v", len(items), items[0].ID, items[len(items)-1].ID)
	}
	if first := items[0]; first.Title != "T1" || first.Language != "" || first.DurationSeconds != 1 || first.CreatedAt.Unix() != 1760000001 ||
		first.AutoDeleteAt != nil || len(first.Tags) != 1 || first.Tags[0] != "a" {
		t.Errorf("first row = %+v", first)
	}
	if api.requests() != 3 {
		t.Fatalf("%d requests, want 3", api.requests())
	}
	if q := api.query(0); len(q) != 0 {
		t.Errorf("first query = %v, want none: no limit and no cursor", q)
	}
	if api.query(1).Get("cursor") != "c1" || api.query(2).Get("cursor") != "c2" {
		t.Errorf("cursors = %q, %q", api.query(1).Get("cursor"), api.query(2).Get("cursor"))
	}
}

func TestAPageSizeIsSentOnEveryRequestAndANegativeOneIsRefused(t *testing.T) {
	api := newListAPI(t, threePages)
	if _, err := drain(api.client(t).Transcripts.List(context.Background(), &ListOptions{PageSize: 3})); err != nil {
		t.Fatal(err)
	}
	for i := range api.requests() {
		if api.query(i).Get("limit") != "3" {
			t.Errorf("request %d: limit = %q", i, api.query(i).Get("limit"))
		}
	}
	before := api.requests()
	_, err := drain(api.client(t).Transcripts.List(context.Background(), &ListOptions{PageSize: -1}))
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	_, err = api.client(t).Transcripts.Page(context.Background(), &PageOptions{PageSize: -5})
	_ = wantCode(t, err, KindPreflight, "invalid_option")
	if api.requests() != before {
		t.Error("a request was sent for a page size that was refused")
	}
}

func TestStoppingTheLoopSendsNoFurtherRequest(t *testing.T) {
	api := newListAPI(t, threePages)
	n := 0
	for item, err := range api.client(t).Transcripts.List(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if item.ID == "tr_002" {
			break
		}
	}
	if api.requests() != 1 {
		t.Errorf("%d requests after breaking out in the first page, want 1", api.requests())
	}
	// breaking at the last row of a page must not fetch the next one either
	for range api.client(t).Transcripts.List(context.Background(), nil) {
		n++
		if n == 5 {
			break
		}
	}
	if api.requests() != 2 {
		t.Errorf("%d requests, want 1 + 1", api.requests())
	}
}

func TestACancelledContextEndsTheWalkBetweenPagesAndBetweenRows(t *testing.T) {
	api := newListAPI(t, threePages)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ids []string
	var failure error
	for item, err := range api.client(t).Transcripts.List(ctx, nil) {
		if err != nil {
			failure = err
			continue
		}
		ids = append(ids, item.ID)
		if item.ID == "tr_003" { // the last row of the first page
			cancel()
		}
	}
	_ = wantCode(t, failure, KindAbort, "aborted")
	if len(ids) != 3 || api.requests() != 1 {
		t.Errorf("ids %v, requests %d: the walk went on after the cancel", ids, api.requests())
	}

	api = newListAPI(t, threePages)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	ids, failure = nil, nil
	for item, err := range api.client(t).Transcripts.List(ctx, nil) {
		if err != nil {
			failure = err
			continue
		}
		ids = append(ids, item.ID)
		cancel() // mid-page
	}
	_ = wantCode(t, failure, KindAbort, "aborted")
	if len(ids) != 1 {
		t.Errorf("ids %v, want only the row before the cancel", ids)
	}

	done, stop := context.WithCancel(context.Background())
	stop()
	_, err := drain(api.client(t).Transcripts.List(done, nil))
	_ = wantCode(t, err, KindAbort, "aborted")
}

func TestAPageThatRepeatsTheCursorItWasSentIsAPaginationLoop(t *testing.T) {
	api := newListAPI(t, func(_ string, query url.Values) (int, string) {
		if query.Get("cursor") == "" {
			return 200, rowsPage(1, 2, "same")
		}
		return 200, rowsPage(3, 4, "same")
	})
	items, err := drain(api.client(t).Transcripts.List(context.Background(), nil))
	e := wantCode(t, err, KindUnexpected, "pagination_loop")
	if len(items) != 4 || api.requests() != 2 {
		t.Errorf("items %d, requests %d: the page that looped is still yielded and nothing is fetched after it", len(items), api.requests())
	}
	if e.Status != 0 || e.Kind.IsPreflight() {
		t.Errorf("error %+v", e)
	}
}

func TestAnErrorEndsTheIteratorOnceAndAZeroServiceYieldsOneConfigurationError(t *testing.T) {
	api := newListAPI(t, func(_ string, query url.Values) (int, string) {
		if query.Get("cursor") == "" {
			return 200, rowsPage(1, 1, "c1")
		}
		return 404, jsonError("not_found", 4040)
	})
	calls := 0
	for _, err := range api.client(t).Transcripts.List(context.Background(), nil) {
		calls++
		if calls == 2 && (err == nil || asError(t, err).Status != 404) {
			t.Errorf("second yield = %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("%d yields, want the row and then the error", calls)
	}
	var zero Client
	for name, seq := range map[string]func() int{
		"transcripts": func() int {
			n := 0
			for _, err := range zero.Transcripts.List(context.Background(), nil) {
				n++
				_ = wantCode(t, err, KindConfiguration, "config_invalid")
			}
			return n
		},
		"deleted": func() int {
			n := 0
			for _, err := range zero.Transcripts.Deleted(context.Background()) {
				n++
				_ = wantCode(t, err, KindConfiguration, "config_invalid")
			}
			return n
		},
		"jobs": func() int {
			n := 0
			for _, err := range zero.Jobs.List(context.Background(), nil) {
				n++
				_ = wantCode(t, err, KindConfiguration, "config_invalid")
			}
			return n
		},
	} {
		if n := seq(); n != 1 {
			t.Errorf("%s: %d yields from a zero client, want 1", name, n)
		}
	}
	_, err := zero.Transcripts.Get(context.Background(), "x")
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
	_, err = zero.Transcripts.Page(context.Background(), nil)
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
	_, err = zero.Jobs.Get(context.Background(), "x")
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
	_, err = zero.Balance(context.Background())
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
	_, err = zero.Account(context.Background())
	_ = wantCode(t, err, KindConfiguration, "config_invalid")
}

func TestTranscriptsPageTakesACursorAndReportsTheNext(t *testing.T) {
	api := newListAPI(t, threePages)
	page, err := api.client(t).Transcripts.Page(context.Background(), &PageOptions{Cursor: "c1", PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || page.Items[0].ID != "tr_004" || page.NextCursor != "c2" {
		t.Errorf("page = %+v", page)
	}
	if q := api.query(0); q.Get("cursor") != "c1" || q.Get("limit") != "3" {
		t.Errorf("query = %v", q)
	}
	last, err := api.client(t).Transcripts.Page(context.Background(), &PageOptions{Cursor: "c2"})
	if err != nil || last.NextCursor != "" || len(last.Items) != 1 {
		t.Errorf("last page = %+v, %v", last, err)
	}
}

func TestAListAnswerTheSDKCannotUseIsBadResponse(t *testing.T) {
	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"empty object": {200, `{}`}, "null": {200, `null`}, "null rows": {200, `{"transcripts":null,"next_cursor":null}`},
		"mistyped": {200, `{"transcripts":[{"id":5}]}`}, "truncated": {200, `{"transcripts":[`},
	} {
		t.Run(name, func(t *testing.T) {
			api := newListAPI(t, func(string, url.Values) (int, string) { return answer.status, answer.body })
			_, err := drain(api.client(t).Transcripts.List(context.Background(), nil))
			e := wantCode(t, err, KindAPI, "bad_response")
			if e.Status != answer.status {
				t.Errorf("status %d, want %d", e.Status, answer.status)
			}
		})
	}
}

func TestTranscriptsGetBuildsATranscriptWithItsFormatters(t *testing.T) {
	body := `{"id":"tr_9","title":"T","language_detected":"en","duration_seconds":3,"speaker_names":{},"tags":[],"created_at":1,"updated_at":2,"auto_delete_at":null,"edited":false,` +
		`"body":{"version":1,"original":{"utterances":[{"speaker":"A","start_ms":0,"end_ms":1500,"text":"hello there"}]},"translations":{}}}`
	api := newListAPI(t, func(path string, _ url.Values) (int, string) {
		if path == "/v1/transcripts/tr_9" {
			return 200, body
		}
		return 404, jsonError("not_found", 4040)
	})
	tr, err := api.client(t).Transcripts.Get(context.Background(), "tr_9")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Language != "en" || tr.Text(nil) != "Speaker A: hello there" {
		t.Errorf("transcript = %+v / %q", tr, tr.Text(nil))
	}
	_, err = api.client(t).Transcripts.Get(context.Background(), "nope")
	if e := asError(t, err); e.Status != 404 {
		t.Errorf("error %+v", e)
	}
	bad := newListAPI(t, func(string, url.Values) (int, string) { return 200, `{"id":"tr_9"}` })
	_, err = bad.client(t).Transcripts.Get(context.Background(), "tr_9")
	_ = wantCode(t, err, KindAPI, "bad_response")
}

func TestDeletedKeepsTheReceiptOpaqueAndTheCauseAsSent(t *testing.T) {
	api := newListAPI(t, func(_ string, query url.Values) (int, string) {
		if query.Get("cursor") == "" {
			return 200, `{"deleted":[{"id":"d1","title":"A","duration_seconds":30,"created_at":1760000000,"deleted_at":1760000500,"cause":"manual",` +
				`"receipt":{"version":1,"type":"deletion","created_at":"2026-09-07T14:53:35Z","items":[{"kind":"transcript","deleted_at":"2026-09-07T14:53:35Z"}]}}],"next_cursor":"n1"}`
		}
		return 200, `{"deleted":[{"id":"d2","title":"B","duration_seconds":40,"created_at":1760000001,"deleted_at":1760000600,"cause":"quarantined","receipt":null}],"next_cursor":null}`
	})
	items, err := drain(api.client(t).Transcripts.Deleted(context.Background()))
	if err != nil || len(items) != 2 {
		t.Fatalf("items %d, err %v", len(items), err)
	}
	first := items[0]
	if first.Cause != "manual" || first.DeletedAt.Unix() != 1760000500 || first.CreatedAt.Unix() != 1760000000 || first.DurationSeconds != 30 {
		t.Errorf("first = %+v", first)
	}
	if first.Receipt["created_at"] != "2026-09-07T14:53:35Z" || first.Receipt["type"] != "deletion" {
		t.Errorf("receipt = %v, want the API's snake_case keys and string timestamps", first.Receipt)
	}
	if items[1].Cause != "quarantined" || items[1].Receipt != nil {
		t.Errorf("second = %+v", items[1])
	}
	if api.query(1).Get("cursor") != "n1" {
		t.Errorf("cursor = %q", api.query(1).Get("cursor"))
	}
}

func jobJSON(id, state, extra string) string {
	return fmt.Sprintf(`{"id":%q,"state":%q,"mode":"standard","declared_seconds":120,"billable_seconds":null,"verified_seconds":null,"actual_seconds":null,`+
		`"created_at":1760000000,"error":null,"failure_kind":null,"retry_of":null,"transcript_id":null,"private_result_available":false,"private_result_expires_at":null,`+
		`"options":{"language_code":null,"speaker_detection":true,"medical_mode":false,"multichannel":false,"translation_targets":["fr"],"keyterms":true,"prompt":false,"auto_generate_insights":null}%s}`, id, state, extra)
}

func TestJobsListSendsActiveAndMapsEveryFieldIncludingFutureStates(t *testing.T) {
	api := newListAPI(t, func(_ string, query url.Values) (int, string) {
		if query.Get("cursor") == "" {
			return 200, `{"jobs":[` + jobJSON("j1", "reviewing", `,"x_new":1`) + `],"next_cursor":"p2"}`
		}
		failed := `{"id":"j2","state":"failed","mode":"standard","declared_seconds":30,"billable_seconds":0,"verified_seconds":29,"actual_seconds":0,"created_at":1760000100,` +
			`"error":"could not decode","failure_kind":"media_undecodable","retry_of":"j0","transcript_id":null,"options":{"language_code":"de","auto_generate_insights":false}}`
		return 200, `{"jobs":[` + failed + `],"next_cursor":null}`
	})
	jobs, err := drain(api.client(t).Jobs.List(context.Background(), &JobListOptions{Active: true}))
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs %d, err %v", len(jobs), err)
	}
	for i := range 2 {
		if api.query(i).Get("active") != "true" {
			t.Errorf("request %d: active = %q", i, api.query(i).Get("active"))
		}
	}
	j := jobs[0]
	if j.State != "reviewing" || j.Mode != "standard" || j.CreatedAt.Unix() != 1760000000 || j.TranscriptID != "" || j.Error != "" || j.RetryOf != "" {
		t.Errorf("first = %+v", j)
	}
	if j.DeclaredSeconds == nil || *j.DeclaredSeconds != 120 || j.BillableSeconds != nil || j.VerifiedSeconds != nil || j.ActualSeconds != nil {
		t.Errorf("seconds = %v %v %v %v: null must stay nil, a number a pointer", j.DeclaredSeconds, j.VerifiedSeconds, j.ActualSeconds, j.BillableSeconds)
	}
	if o := j.Options; o.LanguageCode != "" || !o.SpeakerDetection || !o.Keyterms || o.Prompt || o.AutoGenerateInsights != nil || len(o.TranslationTargets) != 1 {
		t.Errorf("options = %+v", o)
	}
	f := jobs[1]
	if f.FailureKind != "media_undecodable" || f.Error != "could not decode" || f.RetryOf != "j0" || f.BillableSeconds == nil || *f.BillableSeconds != 0 ||
		f.Options.LanguageCode != "de" || f.Options.AutoGenerateInsights == nil || *f.Options.AutoGenerateInsights {
		t.Errorf("second = %+v", f)
	}
}

func TestJobsListWithoutActiveSendsNoFilterAndGetReadsOneJob(t *testing.T) {
	api := newListAPI(t, func(path string, _ url.Values) (int, string) {
		if path == "/v1/jobs" {
			return 200, `{"jobs":[],"next_cursor":null}`
		}
		return 200, jobJSON("j7", "done", `,"transcript_id":"tr_7"`)[:0] + strings.Replace(jobJSON("j7", "done", ""), `"transcript_id":null`, `"transcript_id":"tr_7"`, 1)
	})
	if _, err := drain(api.client(t).Jobs.List(context.Background(), nil)); err != nil {
		t.Fatal(err)
	}
	if _, present := api.query(0)["active"]; present {
		t.Errorf("query = %v", api.query(0))
	}
	job, err := api.client(t).Jobs.Get(context.Background(), "j7")
	if err != nil || job.ID != "j7" || job.TranscriptID != "tr_7" || job.State != "done" {
		t.Errorf("job = %+v, %v", job, err)
	}
	bad := newListAPI(t, func(string, url.Values) (int, string) { return 200, `{"id":"j7"}` })
	_, err = bad.client(t).Jobs.Get(context.Background(), "j7")
	_ = wantCode(t, err, KindAPI, "bad_response")
	_, err = drain(newListAPI(t, func(string, url.Values) (int, string) { return 200, `{"jobs":[{"id":3}]}` }).client(t).Jobs.List(context.Background(), nil))
	_ = wantCode(t, err, KindAPI, "bad_response")
}

func accountJSON(extra string) string {
	return `{"balance_seconds":%d,"held_seconds":%d,"card_on_file":true,"data_region":"eu","credits_expire_at":%s,` +
		`"limits":{"active_jobs":1,"max_active_jobs":50,"max_daily_seconds":36000,"used_today_seconds":600}` + extra + `}`
}

func TestBalanceTruncatesTowardZeroAndIsNetOfHolds(t *testing.T) {
	for _, tc := range []struct {
		seconds, held, minutes int
	}{{125, 30, 2}, {59, 0, 0}, {60, 0, 1}, {0, 0, 0}, {-1, 0, 0}, {-59, 5, 0}, {-60, 0, -1}, {-61, 0, -1}, {-3601, 0, -60}} {
		api := newListAPI(t, func(string, url.Values) (int, string) {
			return 200, fmt.Sprintf(accountJSON(""), tc.seconds, tc.held, "1760000000")
		})
		b, err := api.client(t).Balance(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if b.Seconds != tc.seconds || b.Minutes != tc.minutes || b.HeldSeconds != tc.held {
			t.Errorf("Balance(%d, held %d) = %+v, want %d minutes with the held seconds as sent, never subtracted", tc.seconds, tc.held, b, tc.minutes)
		}
		if b.ExpiresAt == nil || b.ExpiresAt.Unix() != 1760000000 || b.PurchasesRemaining != nil {
			t.Errorf("ExpiresAt %v, PurchasesRemaining %v", b.ExpiresAt, b.PurchasesRemaining)
		}
	}
}

func TestBalanceAndAccountReadTheOptionalFieldsAsNilWhenAbsent(t *testing.T) {
	api := newListAPI(t, func(string, url.Values) (int, string) {
		return 200, fmt.Sprintf(accountJSON(`,"purchases_remaining_in_window":0,"x_new":{"a":1}`), 90, 10, "null")
	})
	client := api.client(t)
	b, err := client.Balance(context.Background())
	if err != nil || b.ExpiresAt != nil || b.PurchasesRemaining == nil || *b.PurchasesRemaining != 0 {
		t.Errorf("balance = %+v, %v: a purchases_remaining_in_window of 0 is present", b, err)
	}
	a, err := client.Account(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.BalanceSeconds != 90 || a.HeldSeconds != 10 || a.CreditsExpireAt != nil || !a.CardOnFile || a.DataRegion != "eu" ||
		a.PurchasesRemainingInWindow == nil || *a.PurchasesRemainingInWindow != 0 ||
		a.Limits.MaxDailySeconds != 36000 || a.Limits.MaxActiveJobs != 50 || a.Limits.ActiveJobs != 1 || a.Limits.UsedTodaySeconds != 600 {
		t.Errorf("account = %+v", a)
	}
	plain := newListAPI(t, func(string, url.Values) (int, string) { return 200, fmt.Sprintf(accountJSON(""), 90, 10, "null") })
	if a, err = plain.client(t).Account(context.Background()); err != nil || a.PurchasesRemainingInWindow != nil {
		t.Errorf("account = %+v, %v", a, err)
	}
}

func TestBalanceAndAccountRefuseAnAnswerWithoutTheirFields(t *testing.T) {
	for name, answer := range map[string]struct {
		status int
		body   string
		onlyAc bool // Balance accepts it
	}{
		"empty": {200, `{}`, false}, "null": {200, `null`, false}, "no held": {200, `{"balance_seconds":5,"limits":{}}`, false},
		"mistyped": {200, `{"balance_seconds":"5","held_seconds":0}`, false}, "truncated": {200, `{"balance_seconds":`, false},
		"no limits": {200, `{"balance_seconds":5,"held_seconds":0}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			api := newListAPI(t, func(string, url.Values) (int, string) { return answer.status, answer.body })
			_, err := api.client(t).Account(context.Background())
			if e := wantCode(t, err, KindAPI, "bad_response"); e.Status != answer.status {
				t.Errorf("Account status %d, want %d", e.Status, answer.status)
			}
			_, err = api.client(t).Balance(context.Background())
			if answer.onlyAc {
				if err != nil {
					t.Errorf("Balance refused %s: %v", answer.body, err)
				}
				return
			}
			_ = wantCode(t, err, KindAPI, "bad_response")
		})
	}
}

func TestBalanceErrorsKeepTheAPIsStatusAndACancelledContextIsAnAbort(t *testing.T) {
	api := newListAPI(t, func(string, url.Values) (int, string) { return 403, jsonError("forbidden", 1030) })
	_, err := api.client(t).Balance(context.Background())
	if e := asError(t, err); e.Status != 403 {
		t.Errorf("error %+v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = api.client(t).Balance(ctx)
	_ = wantCode(t, err, KindAbort, "aborted")
	_, err = api.client(t).Account(ctx)
	_ = wantCode(t, err, KindAbort, "aborted")
	_, err = api.client(t).Jobs.Get(ctx, "x")
	_ = wantCode(t, err, KindAbort, "aborted")
	_, err = api.client(t).Transcripts.Get(ctx, "x")
	_ = wantCode(t, err, KindAbort, "aborted")
	_, err = api.client(t).Transcripts.Page(ctx, nil)
	_ = wantCode(t, err, KindAbort, "aborted")
}

func TestTranscriptPageSizeUsesZeroAsUnsetAndRejectsNegative(t *testing.T) {
	for name, run := range map[string]func(*Client, int) error{
		"list": func(c *Client, size int) error {
			_, err := drain(c.Transcripts.List(context.Background(), &ListOptions{PageSize: size}))
			return err
		},
		"page": func(c *Client, size int) error {
			_, err := c.Transcripts.Page(context.Background(), &PageOptions{PageSize: size})
			return err
		},
	} {
		for _, size := range []int{-1, 0} {
			t.Run(fmt.Sprintf("%s/%d", name, size), func(t *testing.T) {
				api := newListAPI(t, func(_ string, query url.Values) (int, string) {
					if _, ok := query["limit"]; ok {
						t.Errorf("zero PageSize sent limit: %v", query)
					}
					return 200, rowsPage(1, 1, "")
				})
				err := run(api.client(t), size)
				if size < 0 {
					assertCoreError(t, err, KindPreflight, "invalid_option", 0)
					if api.requests() != 0 {
						t.Errorf("invalid PageSize sent %d requests", api.requests())
					}
				} else if err != nil || api.requests() != 1 {
					t.Errorf("unset PageSize: %d requests, %v", api.requests(), err)
				}
			})
		}
	}
}

func TestReadIDsAreRejectedBeforeAnyRequest(t *testing.T) {
	for name, run := range map[string]func(*Client, string) error{
		"transcript": func(c *Client, id string) error { _, err := c.Transcripts.Get(context.Background(), id); return err },
		"job":        func(c *Client, id string) error { _, err := c.Jobs.Get(context.Background(), id); return err },
		"wait":       func(c *Client, id string) error { _, err := c.Jobs.Wait(context.Background(), id, nil); return err },
	} {
		for _, id := range []string{"", ".", ".."} {
			t.Run(fmt.Sprintf("%s/%q", name, id), func(t *testing.T) {
				srv := newScriptedServer(t, okBody)
				c := newTestClient(t, srv.URL, newFakeClock())
				assertCoreError(t, run(c, id), KindPreflight, "invalid_option", 0)
				if srv.count() != 0 {
					t.Errorf("invalid id sent %d requests", srv.count())
				}
			})
		}
	}
}

func TestCanceledCoreEntrypointsSendNoRequest(t *testing.T) {
	for name, run := range map[string]func(context.Context, *Client) error{
		"account":         func(ctx context.Context, c *Client) error { _, err := c.Account(ctx); return err },
		"balance":         func(ctx context.Context, c *Client) error { _, err := c.Balance(ctx); return err },
		"transcript list": func(ctx context.Context, c *Client) error { _, err := drain(c.Transcripts.List(ctx, nil)); return err },
		"transcript page": func(ctx context.Context, c *Client) error { _, err := c.Transcripts.Page(ctx, nil); return err },
		"deleted":         func(ctx context.Context, c *Client) error { _, err := drain(c.Transcripts.Deleted(ctx)); return err },
		"jobs list":       func(ctx context.Context, c *Client) error { _, err := drain(c.Jobs.List(ctx, nil)); return err },
		"job get":         func(ctx context.Context, c *Client) error { _, err := c.Jobs.Get(ctx, "job_1"); return err },
		"job wait":        func(ctx context.Context, c *Client) error { _, err := c.Jobs.Wait(ctx, "job_1", nil); return err },
		"transcript get":  func(ctx context.Context, c *Client) error { _, err := c.Transcripts.Get(ctx, "tr_1"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			srv := newScriptedServer(t, okBody)
			c := newTestClient(t, srv.URL, newFakeClock())
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := run(ctx, c)
			e := wantCoreError(t, err, KindAbort, "aborted", 0)
			if !errors.Is(err, context.Canceled) || e.IdempotencyKey != "" || srv.count() != 0 {
				t.Errorf("cancel: %v, key %q, requests %d", err, e.IdempotencyKey, srv.count())
			}
		})
	}
}

func TestTranscriptPageRejectsRepeatedNonemptyCursor(t *testing.T) {
	for _, tc := range []struct {
		cursor, next string
		loop         bool
	}{
		{"same", "same", true}, {" ", " ", true}, {"\t", "\t", true}, {"\u2003", "\u2003", true},
		{"", "", false}, {"", "next", false}, {"sent", "next", false}, {"sent", "", false},
	} {
		t.Run(fmt.Sprintf("%q/%q", tc.cursor, tc.next), func(t *testing.T) {
			api := newListAPI(t, func(_ string, query url.Values) (int, string) {
				if query.Get("cursor") != tc.cursor {
					t.Errorf("sent cursor = %q", query.Get("cursor"))
				}
				return 200, rowsPage(1, 1, tc.next)
			})
			page, err := api.client(t).Transcripts.Page(context.Background(), &PageOptions{Cursor: tc.cursor})
			if tc.loop {
				assertCoreError(t, err, KindUnexpected, "pagination_loop", 0)
				failure := asError(t, err)
				if failure.Message != "The API returned the same page cursor it was sent; stopping instead of repeating the page." {
					t.Errorf("message = %q", failure.Message)
				}
				if page != nil {
					t.Error("loop returned a page")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if page.NextCursor != tc.next || len(page.Items) != 1 {
					t.Errorf("page = %+v", page)
				}
			}
			if api.requests() != 1 {
				t.Errorf("requests = %d", api.requests())
			}
		})
	}
}

func TestTranscriptRowsKeepTheirOwnRaw(t *testing.T) {
	for _, unknown := range []string{`"x_future":{"nested":[1,null,{"k":"v"}]},`, ""} {
		t.Run(unknown, func(t *testing.T) {
			row := `{` + unknown + `"id":"tr_1","title":"T","duration_seconds":5,"created_at":1760000000,"tags":[],"deleted_at":1760000010,"cause":"manual","receipt":{"x_receipt":{"future":true}}}`
			api := newListAPI(t, func(path string, _ url.Values) (int, string) {
				if path == "/v1/transcripts/deleted" {
					return 200, `{"deleted":[` + row + `],"next_cursor":null}`
				}
				return 200, `{"transcripts":[` + row + `],"next_cursor":null}`
			})
			c := api.client(t)
			page, err := c.Transcripts.Page(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("rows = %d", len(page.Items))
			}
			if got := nestedRaw(t, page.Items[0]); got != row {
				t.Errorf("summary Raw = %s, want %s", got, row)
			}
			rows, err := drain(c.Transcripts.Deleted(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("deleted rows = %d", len(rows))
			}
			if got := nestedRaw(t, rows[0]); got != row {
				t.Errorf("deleted Raw = %s, want %s", got, row)
			}
		})
	}
}
