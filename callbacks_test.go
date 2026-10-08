package hushscript

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

func TestAPanicInOnResumeTokenReachesTheCallerWithItsValue(t *testing.T) {
	r := newRig(t)
	sentinel := &callbackSentinel{"token"}
	raised := raisedBy(func() {
		_, _ = r.transcribe(&TranscribeOptions{OnResumeToken: func(string) { panic(sentinel) }})
	})
	if raised != any(sentinel) {
		t.Fatalf("raised %#v, want the callback's own value", raised)
	}
}

func TestAPanicInOnProgressOnAnUploadWorkerReachesTheCallerWithItsValue(t *testing.T) {
	for name, opts := range map[string]TranscribeOptions{
		"one worker":   {Concurrency: 1},
		"four workers": {Concurrency: 4},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			sentinel := &callbackSentinel{"worker"}
			opts.OnProgress = func(p Progress) {
				if p.Stage == StageUploading && p.BytesSent > 0 {
					panic(sentinel)
				}
			}
			var err error
			raised := raisedBy(func() { _, err = r.transcribe(&opts) })
			if raised != any(sentinel) {
				t.Fatalf("raised %#v, error %v; want the callback's own value", raised, err)
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

func TestJobsWaitProgressPanicsPreserveErrorAndNonErrorValues(t *testing.T) {
	for _, errorValue := range []bool{false, true} {
		t.Run(map[bool]string{false: "value", true: "error"}[errorValue], func(t *testing.T) {
			r := newRig(t)
			r.api.jobStates = []string{"processing", "done"}
			var sentinel any = &callbackSentinel{"jobs wait"}
			if errorValue {
				sentinel = errors.New("caller callback error")
			}
			raised := raisedBy(func() {
				_, _ = r.client.Jobs.Wait(context.Background(), "job_1", &WaitOptions{OnProgress: func(Progress) { panic(sentinel) }})
			})
			if raised != sentinel {
				t.Fatalf("panic = %#v, want original %#v", raised, sentinel)
			}
		})
	}
}

func TestResumeProgressPanicsPropagateTheirOriginalValue(t *testing.T) {
	for _, stage := range []ProgressStage{StageUploading, StageProcessing} {
		for _, errorValue := range []bool{false, true} {
			t.Run(string(stage)+"/"+map[bool]string{false: "value", true: "error"}[errorValue], func(t *testing.T) {
				r := newRig(t)
				var token string
				mustTranscribe(t, r, &TranscribeOptions{IdempotencyKey: strings.Repeat("k", 16), OnResumeToken: func(s string) { token = s }})
				if stage == StageUploading {
					r.api.jobStates = []string{"uploading"}
				} else {
					r.api.jobStates = []string{"processing"}
				}
				var sentinel any = &callbackSentinel{"resume"}
				if errorValue {
					sentinel = errors.New("caller callback error")
				}
				raised := raisedBy(func() {
					_, _ = r.client.Resume(context.Background(), token, &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav)), OnProgress: func(p Progress) {
						if p.Stage == stage {
							panic(sentinel)
						}
					}})
				})
				if raised != sentinel {
					t.Fatalf("panic = %#v, want original %#v", raised, sentinel)
				}
			})
		}
	}
}

func TestResumeWorkerPanicsPropagateTheirOriginalValue(t *testing.T) {
	for _, mode := range []string{"panic value", "panic error"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			token := stalledUpload(t, r)
			opts := &ResumeOptions{Reader: bytes.NewReader(r.wav), Size: int64(len(r.wav)), Concurrency: 0}
			var sentinel any = &callbackSentinel{"resume worker"}
			if mode == "panic error" {
				sentinel = errors.New("caller callback error")
			}
			opts.OnProgress = func(p Progress) {
				if p.BytesSent > 0 {
					panic(sentinel)
				}
			}
			raised := raisedBy(func() { _, _ = r.client.Resume(context.Background(), token, opts) })
			if raised != sentinel {
				t.Errorf("panic = %#v, want original %#v", raised, sentinel)
			}
		})
	}
}
