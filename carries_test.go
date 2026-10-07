package hushscript

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestTheWrongStateHintIsOnlyOnJobAndUploadOperations(t *testing.T) {
	body := []byte(jsonError("wrong_state", 4001))
	for name, tc := range map[string]struct {
		method, path string
		hinted       bool
	}{
		"job read":     {http.MethodGet, "/v1/jobs/j1", true},
		"job list":     {http.MethodGet, "/v1/jobs", true},
		"upload parts": {http.MethodPut, "/v1/uploads/j1/parts/2", true},
		"complete":     {http.MethodPost, "/v1/uploads/j1/complete", true},
		"claim":        {http.MethodPost, "/v1/agent/accounts/sg_1/claim", false},
		"account":      {http.MethodGet, "/v1/account", false},
		"purchase":     {http.MethodPost, "/v1/billing/purchase", false},
	} {
		t.Run(name, func(t *testing.T) {
			e := errorFromResponse(http.StatusConflict, http.Header{}, body, tc.method, tc.path, redactor{})
			if e.Code != "wrong_state" || e.Kind != KindConflict {
				t.Fatalf("error = %+v", e)
			}
			if got := strings.Contains(e.Hint, "Fetch the job"); got != tc.hinted || (!tc.hinted && e.Hint != "") {
				t.Errorf("hint %q, want hinted=%v", e.Hint, tc.hinted)
			}
		})
	}
}

func TestTheMissingTokenHintNamesAccountMCP(t *testing.T) {
	t.Setenv("HUSHSCRIPT_TOKEN", "")
	_, err := New()
	if hint := asError(t, err).Hint; !strings.Contains(hint, "Account → MCP (app.hushscript.com/account/mcp).") || strings.Contains(hint, "Connected apps") {
		t.Errorf("hint = %q", hint)
	}
}

func TestBoolPointsAtItsArgument(t *testing.T) {
	if f, tr := Bool(false), Bool(true); f == nil || tr == nil || *f || !*tr || f == tr {
		t.Errorf("Bool(false) = %v, Bool(true) = %v", f, tr)
	}
}

func TestATranscriptDurationIsAWholeNumberOfSeconds(t *testing.T) {
	var tr Transcript
	raw := `{"id":"tr_1","title":"T","language_detected":null,"duration_seconds":61,"created_at":1,"updated_at":2,"body":{"original":{"utterances":[]}}}`
	if err := tr.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if _, isInt := any(tr.DurationSeconds).(int); !isInt || tr.DurationSeconds != 61 {
		t.Errorf("DurationSeconds = %v (%T), want the int 61", tr.DurationSeconds, tr.DurationSeconds)
	}
	fractional := strings.Replace(raw, `"duration_seconds":61`, `"duration_seconds":61.5`, 1)
	api := newListAPI(t, func(string, url.Values) (int, string) { return 200, fractional })
	_, err := api.client(t).Transcripts.Get(context.Background(), "tr_1")
	_ = wantCode(t, err, KindAPI, "bad_response")
}

func TestAMissingCreatedAtIsBadResponseNotAZeroTime(t *testing.T) {
	transcript := `{"id":"tr_1","title":"T","language_detected":null,"duration_seconds":3,"updated_at":2,"body":{"original":{"utterances":[]}}}`
	job := strings.Replace(jobJSON("j1", "done", ""), `"created_at":1760000000,`, "", 1)
	for name, tc := range map[string]struct {
		body string
		call func(*Client) error
	}{
		"transcript": {transcript, func(c *Client) error { _, err := c.Transcripts.Get(context.Background(), "tr_1"); return err }},
		"job":        {job, func(c *Client) error { _, err := c.Jobs.Get(context.Background(), "j1"); return err }},
		"job row": {`{"jobs":[` + job + `],"next_cursor":null}`, func(c *Client) error {
			_, err := drain(c.Jobs.List(context.Background(), nil))
			return err
		}},
		"transcript row": {`{"transcripts":[{"id":"tr_1","title":"T","duration_seconds":3,"tags":[]}],"next_cursor":null}`, func(c *Client) error {
			_, err := drain(c.Transcripts.List(context.Background(), nil))
			return err
		}},
		"deleted row": {`{"deleted":[{"id":"d1","title":"A","duration_seconds":30,"deleted_at":2,"cause":"manual","receipt":null}],"next_cursor":null}`, func(c *Client) error {
			_, err := drain(c.Transcripts.Deleted(context.Background()))
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			api := newListAPI(t, func(string, url.Values) (int, string) { return 200, tc.body })
			_ = wantCode(t, tc.call(api.client(t)), KindAPI, "bad_response")
		})
	}
}
