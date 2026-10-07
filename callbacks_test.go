package hushscript

import (
	"testing"
)

// Spec 9.2 (R63): what a caller's callback raises propagates unchanged. Go callbacks cannot return an
// error, so what they can raise is a panic; the SDK recovers none of them and wraps none.

type callbackSentinel struct{ where string }

// raisedBy runs f and returns what it panicked with, nil when it returned.
func raisedBy(f func()) (raised any) {
	defer func() { raised = recover() }()
	f()
	return nil
}

func TestAPanicInOnResumeTokenLeavesTranscribeUnchangedBeforeAnythingElseIsSent(t *testing.T) {
	r := newRig(t)
	sentinel := &callbackSentinel{"token"}
	raised := raisedBy(func() {
		_, _ = r.transcribe(&TranscribeOptions{OnResumeToken: func(string) { panic(sentinel) }})
	})
	if raised != any(sentinel) {
		t.Fatalf("raised %#v, want the callback's own value", raised)
	}
	if r.api.count("create") != 1 || r.api.count("complete") != 0 || r.api.count("r2") != 0 {
		t.Errorf("create %d, parts %d, complete %d", r.api.count("create"), r.api.count("r2"), r.api.count("complete"))
	}
}

func TestAPanicInOnProgressOnAnUploadWorkerReachesTheCallerWithItsValueAndStopsTheUpload(t *testing.T) {
	for name, opts := range map[string]TranscribeOptions{
		"one worker":   {Concurrency: 1},
		"four workers": {Concurrency: 4},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			sentinel := &callbackSentinel{"worker"}
			reports := 0
			opts.OnProgress = func(p Progress) {
				reports++
				if p.Stage == StageUploading && p.BytesSent > 0 {
					panic(sentinel)
				}
			}
			var err error
			raised := raisedBy(func() { _, err = r.transcribe(&opts) })
			if raised != any(sentinel) {
				t.Fatalf("raised %#v, error %v; want the callback's own value", raised, err)
			}
			if r.api.count("complete") != 0 {
				t.Error("the upload went on to complete after the callback panicked")
			}
			if reports != 2 {
				t.Errorf("OnProgress ran %d times: once at the start, once to panic, never again", reports)
			}
		})
	}
}

func TestAPanicInOnProgressWhileProcessingReachesTheCallerWithItsValue(t *testing.T) {
	r := newRig(t)
	sentinel := &callbackSentinel{"processing"}
	raised := raisedBy(func() {
		_, _ = r.transcribe(&TranscribeOptions{OnProgress: func(p Progress) {
			if p.Stage == StageProcessing {
				panic(sentinel)
			}
		}})
	})
	if raised != any(sentinel) {
		t.Fatalf("raised %#v, want the callback's own value", raised)
	}
}

// A panic whose value is itself an SDK error is still the caller's value: it is not annotated with a key
// or a token on the way out, and nothing replaces it.
func TestAPanicWithAnSDKErrorValueIsNotAnnotatedOrReplaced(t *testing.T) {
	r := newRig(t)
	own := &Error{Code: "mine", Kind: KindConflict}
	raised := raisedBy(func() {
		_, _ = r.transcribe(&TranscribeOptions{OnResumeToken: func(string) { panic(own) }})
	})
	if raised != any(own) || own.Code != "mine" || own.IdempotencyKey != "" || own.ResumeToken != "" {
		t.Errorf("raised %#v, error %+v", raised, own)
	}
}
