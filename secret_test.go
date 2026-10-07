package hushscript

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/stretchr/testify/assert"
)

const (
	// receivedText is the token a failed rotation hands back on the error.
	receivedText = "hsr1:eu:pat.cccccccccccccccccccccccccccccccc.dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd" //nolint:gosec // a made-up token
	// signatureValue is the signature of a presigned storage URL.
	signatureValue = "5ec2e7aa11b4c0de99f0aa5551c3b1f4"
	signedPartURL  = "https://r2.example/obj/1?X-Amz-Signature=" + signatureValue + "&X-Amz-Expires=86400"
)

var leakVerbs = []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%t", "%e", "%c", "%U", "%-20s", "%10v"}

// leakMarkers are the texts no output may hold: every token and signature, whole and as its secret
// half, in hex either case, and the decimal bytes a %d of a byte slice prints.
var leakTexts = leakMarkers()

func leakMarkers() []string {
	var markers []string
	for _, text := range []string{testToken, newTokenText, receivedText, signatureValue, signedPartURL} {
		markers = append(markers, text, hex.EncodeToString([]byte(text)), strings.ToUpper(hex.EncodeToString([]byte(text))))
		if _, tail, found := strings.Cut(text, "pat."); found {
			markers = append(markers, tail, hex.EncodeToString([]byte(tail)), strings.ToUpper(hex.EncodeToString([]byte(tail))))
		}
		decimal := fmt.Sprintf("%d", []byte(text)[:12])
		markers = append(markers, strings.TrimSuffix(decimal, "]"))
	}
	return markers
}

// captureT is the TestingT of a testify assertion whose failure message the test wants to read.
type captureT struct{ out []string }

func (c *captureT) Errorf(format string, args ...any) {
	c.out = append(c.out, fmt.Sprintf(format, args...))
}

// channels renders v through every way a value reaches a human or a log: fmt with every verb,
// bare and nested, encoding/json and gob, slog's text and JSON handlers, spew with and without
// methods, and the message testify prints when an equality assertion against peer fails.
func channels(v, peer any) map[string][]string {
	out := map[string][]string{}
	nested := []any{
		v, []any{v}, map[string]any{"k": v}, struct{ V any }{v}, &struct{ V any }{v}, [1]any{v},
	}
	for _, wrapped := range nested {
		for _, verb := range leakVerbs {
			out["fmt"] = append(out["fmt"], fmt.Sprintf(verb, wrapped))
		}
		out["fmt"] = append(out["fmt"], fmt.Sprint(wrapped), fmt.Sprintln(wrapped))
		encoded, err := json.Marshal(wrapped)
		out["json"] = append(out["json"], string(encoded), fmt.Sprint(err))
	}
	for _, encodable := range []any{v, &v} {
		var buffer bytes.Buffer
		err := gob.NewEncoder(&buffer).Encode(encodable)
		out["gob"] = append(out["gob"], buffer.String(), fmt.Sprint(err))
	}
	for name, handler := range map[string]func(*bytes.Buffer) slog.Handler{
		"slog json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		"slog text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
	} {
		var buffer bytes.Buffer
		logger := slog.New(handler(&buffer))
		logger.Info("m", "v", v)
		logger.Info("m", slog.Any("v", v), slog.Group("g", slog.Any("v", v)))
		out[name] = append(out[name], buffer.String())
	}
	cleanConfig := spew.ConfigState{Indent: " ", MaxDepth: 10, SortKeys: true}
	rawConfig := spew.ConfigState{Indent: " ", MaxDepth: 10, SortKeys: true, DisableMethods: true, DisablePointerAddresses: true}
	out["spew"] = append(out["spew"], spew.Sdump(v), cleanConfig.Sdump(v), cleanConfig.Sprintf("%#v %+v", v, v))
	out["spew without methods"] = append(out["spew without methods"], rawConfig.Sdump(v), rawConfig.Sprintf("%#v %+v", v, v))
	recorder := &captureT{}
	assert.Equal(recorder, v, peer)
	assert.NotEqual(recorder, v, v)
	out["testify"] = append(out["testify"], recorder.out...)
	return out
}

// leaksIn names the channels whose output holds a marker.
func leaksIn(rendered map[string][]string) []string {
	var found []string
	for channel, texts := range rendered {
		for _, text := range texts {
			if marker, leaked := firstMarker(text); leaked {
				found = append(found, channel+": "+marker[:min(len(marker), 12)]+"...")
				break
			}
		}
	}
	sort.Strings(found)
	return found
}

func firstMarker(text string) (string, bool) {
	for _, marker := range leakTexts {
		if strings.Contains(text, marker) {
			return marker, true
		}
	}
	return "", false
}

type (
	canaryString   struct{ token string }
	canaryBytes    struct{ token []byte }
	canaryPointer  struct{ token *string }
	canaryBox      struct{ box *struct{ text string } }
	canaryExported struct{ Token string }
	// canaryMethods is the shape of the old design: a redacting type that fmt and testify's spew
	// never ask, because it sits in an unexported field.
	canaryMethods struct{ token redactingString }
	canarySecret  struct{ token secret }
)

type redactingString string

func (redactingString) String() string { return "[redacted]" }

func (redactingString) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted]")) }

// The harness has to be able to fail: each way of holding a token that the root cause names must leak
// through the channels that reach it, and the secret type must leak through none.
func TestTheLeakHarnessSeesEveryWayOfHoldingATokenThatTheSecretTypeClosesOff(t *testing.T) {
	text := testToken
	leaky := map[string]struct {
		subject, peer any
		channels      []string
	}{
		"string":          {canaryString{text}, canaryString{"other"}, []string{"fmt", "spew", "spew without methods", "testify"}},
		"bytes":           {canaryBytes{[]byte(text)}, canaryBytes{}, []string{"fmt", "spew", "spew without methods"}},
		"pointer":         {canaryPointer{&text}, canaryPointer{}, []string{"spew", "spew without methods", "testify"}},
		"boxed text":      {canaryBox{&struct{ text string }{text}}, canaryBox{}, []string{"fmt", "spew", "spew without methods", "testify"}},
		"exported field":  {canaryExported{text}, canaryExported{}, []string{"fmt", "json", "gob", "slog json", "slog text", "spew", "testify"}},
		"methods only":    {canaryMethods{redactingString(text)}, canaryMethods{}, []string{"fmt", "spew without methods", "testify"}},
		"exported (ptr)":  {&canaryExported{text}, &canaryExported{}, []string{"json", "gob", "slog json", "slog text"}},
		"slice of tokens": {[]string{text}, []string{}, []string{"fmt", "json", "spew", "testify"}},
	}
	for name, tc := range leaky {
		leaked := strings.Join(leaksIn(channels(tc.subject, tc.peer)), "\n")
		for _, channel := range tc.channels {
			if !strings.Contains(leaked, channel+":") {
				t.Errorf("%s: the harness did not see the token leak through %q (saw %q)", name, channel, leaked)
			}
		}
	}
	if leaked := leaksIn(channels(canarySecret{newSecret(text)}, canarySecret{newSecret("other")})); len(leaked) != 0 {
		t.Errorf("a secret leaked: %v", leaked)
	}
}

func TestASecretShowsTheMarkerEverywhereAndItsTextOnlyToReveal(t *testing.T) {
	s := newSecret(testToken)
	if s.reveal() != testToken || s.isEmpty() {
		t.Fatal("the text is gone")
	}
	var zero secret
	if zero.reveal() != "" || !zero.isEmpty() || !newSecret("").isEmpty() {
		t.Error("the zero secret is not empty")
	}
	for _, verb := range leakVerbs {
		if got := fmt.Sprintf(verb, s); got != redactedToken {
			t.Errorf("%s printed %q", verb, got)
		}
	}
	if s.String() != redactedToken || s.GoString() != redactedToken {
		t.Error("String or GoString shows more than the marker")
	}
	encoded, err := json.Marshal(struct{ S secret }{s})
	if err != nil || string(encoded) != `{"S":"`+redactedToken+`"}` {
		t.Errorf("json = %s, %v", encoded, err)
	}
	asText, err := s.MarshalText()
	if err != nil || string(asText) != redactedToken {
		t.Errorf("MarshalText = %q, %v", asText, err)
	}
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(struct{ S secret }{s}); err != nil || strings.Contains(buffer.String(), testSecret) || !strings.Contains(buffer.String(), redactedToken) {
		t.Errorf("gob = %q, %v", buffer.String(), err)
	}
	var logged bytes.Buffer
	slog.New(slog.NewTextHandler(&logged, nil)).Info("m", "token", s)
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("m", "token", s)
	if strings.Contains(logged.String(), testSecret) || !strings.Contains(logged.String(), redactedToken) {
		t.Errorf("slog = %s", logged.String())
	}
}

func TestAnErrorAndAClientLogAsTheirRedactedText(t *testing.T) {
	e := newSDKError(KindNetwork, "network_error", "boom "+testToken, errors.New("dial "+testToken), newRedactor(testToken), hintInputs{})
	c := newTestClient(t, "http://127.0.0.1:1", newFakeClock())
	for name, value := range map[string]any{"error": e, "error value": *e, "client": c, "client value": *c} {
		var buffer bytes.Buffer
		slog.New(slog.NewJSONHandler(&buffer, nil)).Info("m", "v", value)
		if _, leaked := firstMarker(buffer.String()); leaked {
			t.Errorf("%s: %s", name, buffer.String())
		}
		// Only the pointer is a slog.LogValuer (a nil one must not panic); a copy of the struct is logged as data.
		if !strings.HasSuffix(name, " value") && !strings.Contains(buffer.String(), "hushscript") {
			t.Errorf("%s logged %s, want its text", name, buffer.String())
		}
	}
}

// leakRig is a client that rotated once (so it holds a current and a past token) and the errors a
// token can end up in.
type leakRig struct {
	client, peer *Client
	response     *http.Response
	subjects     map[string][2]any
}

func newLeakRig(t *testing.T) *leakRig {
	t.Helper()
	srv := newRouteServer(t, func(rec recorded, nth int) step {
		switch {
		case rec.path == "/v1/agent/credentials/rotate" && nth == 1:
			return rotateAnswer("null")
		case rec.path == "/v1/agent/credentials/rotate":
			return ok(`{"pat":"` + receivedText + `","pat_id":"pat_3","scopes":[],"expires_at":"soon"}`)
		}
		return ok(`{"balance_seconds":600,"held_seconds":0,"limits":{}}`)
	})
	c := srv.client(t)
	if _, err := c.Agent.RotateToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, receivedErr := c.Agent.RotateToken(context.Background())
	received := wantCode(t, receivedErr, KindOutcomeUnknown, "outcome_unknown")
	if received.ReceivedToken() != receivedText {
		t.Fatalf("the received token is %q", received.ReceivedToken())
	}

	peer := newTestClient(t, "http://127.0.0.1:2", newFakeClock(), WithToken("hsr1:us:pat.ee.ff"))

	sentinel := errors.New("sentinel")
	echo := &echoingError{text: "transport said: Authorization: Bearer " + testToken + " then " + newTokenText + " and " + receivedText}
	var calls atomic.Int32
	echoing := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithMaxRetries(0),
		WithHTTPClient(failingAfterBody(&calls, fmt.Errorf("%w: %w", sentinel, echo))))
	_, echoed := do(t, echoing, http.MethodGet, "/v1/account", "")
	echoedErr := asError(t, echoed)

	signedReq, err := http.NewRequestWithContext(withDirectStorage(context.Background(), 1), http.MethodPut, deadAddress(t)+"/part?X-Amz-Signature="+signatureValue, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	_, signed := send(c, signedReq)
	signedErr := asError(t, signed)

	apiErr := errorFromResponse(http.StatusBadRequest, http.Header{}, []byte(`{"error":"invalid_request","code":1,"detail":"`+newTokenText+`"}`), http.MethodGet, "/v1/account", c.core.cfg.redactor())

	resp, err := c.API().GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	peerResp := &http.Response{Status: "other"}

	pages := newPartPages(c.core, "j1", &uploadPlan{urls: []partURL{{n: 1, url: newSecret(signedPartURL)}}})
	peerPages := newPartPages(peer.core, "j2", &uploadPlan{urls: []partURL{{n: 9, url: newSecret("https://r2.example/9")}}})

	other := &Error{Code: "other"}
	subjects := map[string][2]any{
		"Client":                        {c, peer},
		"Client value":                  {*c, *peer},
		"Jobs":                          {c.Jobs, peer.Jobs},
		"Jobs pointer":                  {&c.Jobs, &peer.Jobs},
		"Transcripts":                   {c.Transcripts, peer.Transcripts},
		"Transcripts pointer":           {&c.Transcripts, &peer.Transcripts},
		"Billing":                       {c.Billing, peer.Billing},
		"Billing pointer":               {&c.Billing, &peer.Billing},
		"Agent":                         {c.Agent, peer.Agent},
		"Agent pointer":                 {&c.Agent, &peer.Agent},
		"API":                           {c.API(), peer.API()},
		"API client interface":          {c.API().ClientInterface, peer.API().ClientInterface},
		"API response":                  {resp, peerResp},
		"core":                          {c.core, peer.core},
		"doer":                          {c.core.doer, peer.core.doer},
		"config":                        {c.core.cfg, peer.core.cfg},
		"config pointer":                {&c.core.cfg, &peer.core.cfg},
		"token box":                     {c.core.cfg.tokens, peer.core.cfg.tokens},
		"redactor":                      {c.core.cfg.redactor(), peer.core.cfg.redactor()},
		"redactor pointer":              {ptr(c.core.cfg.redactor()), ptr(peer.core.cfg.redactor())},
		"secret":                        {c.core.cfg.token, peer.core.cfg.token},
		"Error (API answer)":            {apiErr, other},
		"Error value (API answer)":      {*apiErr, *other},
		"Error (echoing transport)":     {echoedErr, other},
		"Error value (echoing)":         {*echoedErr, *other},
		"cause (echoing transport)":     {errors.Unwrap(echoedErr), errors.New("other")},
		"Error (received token)":        {received, other},
		"Error value (received token)":  {*received, *other},
		"cause (received token)":        {errors.Unwrap(received), errors.New("other")},
		"Error (signed URL)":            {signedErr, other},
		"Error value (signed URL)":      {*signedErr, *other},
		"cause (signed URL)":            {errors.Unwrap(signedErr), errors.New("other")},
		"Transcript":                    {Transcript{ID: "t1"}, Transcript{ID: "t2"}},
		"part pages":                    {pages, peerPages},
		"part pages value":              {*pages.pages[0], *peerPages.pages[0]},
		"upload plan":                   {&uploadPlan{urls: []partURL{{n: 1, url: newSecret(signedPartURL)}}}, &uploadPlan{}},
		"part URL":                      {partURL{n: 1, url: newSecret(signedPartURL)}, partURL{}},
		"annotator":                     {&annotator{core: c.core}, &annotator{}},
		"redacted cause":                {newRedactedCause(echo, c.core.cfg.redactor()), newRedactedCause(errors.New("other"), redactor{})},
		"redacted cause error in error": {fmt.Errorf("wrapped: %w", echoedErr), errors.New("other")},
	}
	return &leakRig{client: c, peer: peer, response: resp, subjects: subjects}
}

func TestNothingThatHoldsATokenOrASignedURLPrintsOneThroughAnyChannel(t *testing.T) {
	rig := newLeakRig(t)
	names := make([]string, 0, len(rig.subjects))
	for name := range rig.subjects {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pair := rig.subjects[name]
		t.Run(name, func(t *testing.T) {
			if leaked := leaksIn(channels(pair[0], pair[1])); len(leaked) != 0 {
				t.Errorf("leaks through %v", leaked)
			}
		})
	}
}

// Every token the client ever held is still redacted after all of that, and the response the caller
// gets back carries no Authorization header.
func TestTheRotatedClientStillRedactsEveryTokenItHeldAndTheResponseKeepsNoAuthorization(t *testing.T) {
	rig := newLeakRig(t)
	r := rig.client.core.cfg.redactor()
	for _, held := range []string{testToken, newTokenText} {
		if out := r.apply("echo " + held + " done"); strings.Contains(out, held) {
			t.Errorf("%q survived redaction: %s", held, out)
		}
	}
	resp := rig.response
	if resp.Request == nil || resp.Request.Header.Get("Authorization") != "" {
		t.Errorf("the response carries its request's Authorization: %v", resp.Request)
	}
}

// holderTypes are the exported struct types with unexported fields, which the leak suite has to
// reach. A new one that is not in the suite fails here.
func TestEveryExportedStructWithHiddenStateIsInTheLeakSuite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	holders := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for _, declaration := range file.Decls {
			general, isGeneral := declaration.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typed := spec.(*ast.TypeSpec) //nolint:forcetypeassert // a type declaration holds type specs
				structure, isStruct := typed.Type.(*ast.StructType)
				if !isStruct || !ast.IsExported(typed.Name.Name) {
					continue
				}
				if hasHiddenState(structure) {
					holders[typed.Name.Name] = true
				}
			}
		}
	}
	rig := newLeakRig(t)
	covered := map[string]bool{}
	for _, pair := range rig.subjects {
		covered[typeName(pair[0])] = true
	}
	var missing []string
	for name := range holders {
		if !covered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("exported struct types with unexported fields that the leak suite does not format: %v", missing)
	}
	for _, must := range []string{"Client", "Error", "JobsService", "TranscriptsService", "BillingService", "AgentService"} {
		if !holders[must] {
			t.Errorf("%s is no longer seen as a holder: the guard is broken", must)
		}
	}
}

func hasHiddenState(structure *ast.StructType) bool {
	for _, field := range structure.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		for _, name := range field.Names {
			if !ast.IsExported(name.Name) {
				return true
			}
		}
	}
	return false
}

func typeName(v any) string {
	kind := reflect.TypeOf(v)
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	return kind.Name()
}
