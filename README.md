# Hushscript Go SDK

The official Go SDK for the [Hushscript](https://hushscript.com) transcription API. It uploads a
file, waits for the transcript and hands it back, and it handles the failures in between so you do
not have to.

## Quick start

```sh
go get github.com/Hushscript/hushscript-go
export HUSHSCRIPT_TOKEN=hsr1:...   # Account → MCP in the Hushscript app
```

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	hushscript "github.com/Hushscript/hushscript-go"
)

func main() {
	client, err := hushscript.New() // reads HUSHSCRIPT_TOKEN
	if err != nil {
		log.Fatal(err)
	}
	transcript, err := client.Transcribe(context.Background(), "meeting.mp3", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(transcript.Text(nil))
	if err := os.WriteFile("meeting.srt", []byte(transcript.SRT(nil)), 0o600); err != nil {
		log.Fatal(err)
	}
}
```

Requires Go 1.25 or newer. Server-side use only: a token must never ship in a browser bundle.

Options that can be off, on or unset are `*bool`. `hushscript.Bool(false)` and `hushscript.Bool(true)`
build one, and nil keeps the default:

```go
transcript, err := client.Transcribe(ctx, "meeting.mp3", &hushscript.TranscribeOptions{
	Speakers: hushscript.Bool(false),
})
```

## Inputs

`Transcribe` takes a path. `TranscribeReader` takes an `io.ReaderAt` and its exact size. For a
forward-only input (a pipe, a network body) use `TranscribeStream`: it reads one part at a time, in
order, and needs the exact `size` and `DurationSeconds`, because a stream can be neither rewound nor
probed.

```go
transcript, err := client.TranscribeStream(ctx, resp.Body, resp.ContentLength, &hushscript.TranscribeOptions{
	DurationSeconds: 1800,
})
```

`Resume` takes the same kinds: `ResumeOptions.Path`, `Reader` with `Size`, or `Stream`, which is read from
where it stands and must hold the size the resume token records.

## Check my balance

```go
balance, err := client.Balance(ctx)
if err != nil {
	return err
}
fmt.Printf("%d minutes to spend\n", balance.Minutes)
```

`balance.Minutes` is what you can spend now. Minutes held for jobs that are still running are
already taken out of it, so do not subtract `HeldSeconds` again.

## List my transcripts

```go
for transcript, err := range client.Transcripts.List(ctx, nil) {
	if err != nil {
		return err
	}
	fmt.Println(transcript.ID, transcript.Title)
}
```

The list holds finished transcripts only. A recording that is still processing is a job, so look for
it with `client.Jobs.List(ctx, &hushscript.JobListOptions{Active: true})`. `client.Jobs.Wait` waits
for one, and `client.Transcripts.Get` returns the full `Transcript` once it is done. Every list is an
`iter.Seq2` that fetches pages as you range over it, checks `ctx` between pages, and yields an error
once and then stops.

## Buy a pack

```go
packs, err := client.Billing.Packs(ctx)
if err != nil {
	return err
}
for _, pack := range packs {
	fmt.Println(pack.ID, pack.Minutes, "minutes", pack.Price.Formatted)
}
purchase, err := client.Buy(ctx, "300min", &hushscript.BuyOptions{AcknowledgeNoRefund: true})
if err != nil {
	return err
}
fmt.Printf("%d minutes to spend now\n", purchase.Minutes)
```

The packs are `45min`, `300min`, `900min`, `1800min` and `6000min`. Prices depend on your country,
so read them from `pack.Price` and never hard-code one. `Price.Formatted` divides by the minor-unit
digits of Stripe's currency table, not by locale data.

`Buy` charges your saved default card, or the only card that can be charged when there is no
default, and returns a `Purchase`: the `Seconds` and `Minutes` you have after it settled. It makes no
request after the charge, so nothing can fail once the money has moved. With no usable card it
returns a `KindPaymentMethodRequired` error and sends nothing: `no_default_card` when there is none
(add one at the URL from `client.Billing.AddCardURL`, or in the app), `default_card_required` when
several cards could be charged and none is the default (pick one with
`client.Billing.SetDefaultCard`).

Show your buyer this notice before you call it, then set `AcknowledgeNoRefund`:

> Minutes are added to your account instantly after payment. All purchases are final and
> non-refundable.

The flag records that the notice was shown. It does not gate the charge, and without it `Buy` still
goes ahead.

### Allow buying first

Buying from code is off until you allow it in three places. If any is missing, the call fails with
`pat_scope_missing`.

1. The token has the permission labeled "Buy minutes and manage your saved payment methods" (the
   `billing` scope), ticked when you create it.
2. The token has "Let this token buy minutes" turned on, on the same "Create a new token" form.
3. "Let connected apps buy minutes" is on for the whole account.

All of it is in the app under Account → MCP.

### When a purchase does not go through

If the card is declined or cannot be charged without you, the call fails with
`pat_purchase_requires_app` (4021, HTTP 403); if the bank wants to confirm the payment, it fails with
`pat_purchase_requires_authentication` (4023, HTTP 403). In both cases nothing was charged: buy that
pack in the app. A key that met 4021 is dead; a new `Buy` call mints a new one.

A purchase is retried at most once, on a network error, a 502 or a 503, with the same idempotency
key, so those retries cannot charge you twice. When `Buy` still gives up after the request was sent
(the connection dropped twice, a 401, a 408, a 409, a 429, a server error, or an answer the SDK
cannot use), the card may have been charged. Every such error carries `IdempotencyKey`; ask again
with it and the purchase can never happen twice:

```go
purchase, err := client.Buy(ctx, "300min", &hushscript.BuyOptions{AcknowledgeNoRefund: true})
var failure *hushscript.Error
if errors.As(err, &failure) && failure.IdempotencyKey != "" {
	switch {
	case failure.Status == 400 || failure.Status == 402 || failure.Status == 404 || failure.Status == 422 ||
		failure.Status == 403 && (failure.NumericCode == 4021 || failure.NumericCode == 4023):
		// Only these refusals charged nothing (4021 and 4023 are 403). A 401 or a 429 does not
		// prove it: the server answers them before it replays a completed purchase, so after the
		// SDK's one retry they can follow a first attempt that charged.
	default:
		// Anything else after the send may have charged: a 401, a 408, a 409, a 429, a 5xx, no
		// status at all (a lost connection) or an unusable answer. Ask again with the same key.
		purchase, err = client.Buy(ctx, "300min", &hushscript.BuyOptions{
			AcknowledgeNoRefund: true,
			IdempotencyKey:      failure.IdempotencyKey,
		})
	}
}
```

A key you pass yourself must be 1 to 128 characters from `A-Za-z0-9_-`. Do not reuse a key for a
different pack.

## Export subtitles

```go
_ = os.WriteFile("meeting.srt", []byte(transcript.SRT(nil)), 0o600)
_ = os.WriteFile("meeting.vtt", []byte(transcript.VTT(&hushscript.FormatOptions{Language: "es"})), 0o600)
_ = os.WriteFile("meeting.txt", []byte(transcript.Text(&hushscript.TextOptions{Timestamps: true})), 0o600)
```

The output is byte for byte what the Hushscript app exports. `SRT` and `VTT` wrap lines into caption
cues by default; pass `Captions: hushscript.Bool(false)` for one cue per utterance.

## Agents: sign up without a human account

An agent can open an account, have it paid for at a hosted checkout page, and claim its own token.
Signup and claim need no token yet, so they are package functions, not methods of a `Client`. They
take the same options as `New` (`WithBaseURL`, `WithHTTPClient` and so on) and ignore a token.

```go
// The server shows each token once, so write every one down the moment it arrives.
keep := func(token string) error {
	file, err := os.CreateTemp(".", ".token-*") // created with mode 0600
	if err != nil {
		return err
	}
	if _, err := file.WriteString(token + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return err
	}
	return os.Rename(file.Name(), "agent-token") // an earlier token is replaced, never truncated first
}

signup, err := hushscript.AgentSignup(ctx, hushscript.AgentSignupRequest{
	Email:       "agent@example.com",
	Pack:        "45min",
	AcceptTerms: "<the policy version you accept>",
	Agent:       hushscript.AgentInfo{Name: "My agent", Platform: "custom"},
})
if err != nil {
	return err
}
fmt.Printf("Pay %s at %s\n", signup.Price.Formatted, signup.CheckoutURL)

// After the checkout is paid:
account, err := hushscript.AgentClaim(ctx, signup.SignupID, signup.ClaimSecret)
if err != nil {
	return err
}
if err := keep(account.Token); err != nil { // before anything else can fail
	return err
}
client, err := hushscript.New(hushscript.WithToken(account.Token))
if err != nil {
	return err
}
next, err := client.Agent.RotateToken(ctx) // revokes the claimed token; the client sends next.Token
if err != nil {
	return err
}
if err := keep(next.Token); err != nil {
	return err
}
```

- `AcceptTerms` is never filled in for you; an empty one is a `KindValidation` error
  (`accept_terms_required`) before any request. A wrong version fails with `policy_version_stale`, and
  the error's `Hint` names the current one.
- The claim answers `wrong_state` (a `KindConflict` error) until the checkout is paid.
- New agent accounts can buy only `45min` and `300min` until earlier purchases settle.
- Keep `ClaimSecret` and every token: the server shows each of them once, and a rotation revokes the
  token it replaces, so lose the new one and the account is out of reach. A claim or a rotation whose
  answer is lost, or comes back without a usable token, is a `KindOutcomeUnknown` error and is never
  retried, because a retry could not succeed; its `Hint` says what to check. When the answer did carry
  a token but not the rest, `Error.ReceivedToken()` returns it so it is not lost; it never appears in a
  printed or encoded error.
- `ClaimSecret`, `AgentAccount.Token` and `RotatedToken.Token` stay readable fields, but printing the result
  with any `fmt` verb (`%v`, `%+v`, `%#v`, `%s`), a `Stringer` call or slog's text handler shows
  `[redacted]` in their place. `json.Marshal` carries the value on purpose, so do not encode these
  results into a log.
- `DryRun: true` validates a signup without opening a checkout.

## Errors

Every error the SDK returns is a `*hushscript.Error`: match it with `errors.As` and branch on `Kind`
or `Code`.

```go
transcript, err := client.Transcribe(ctx, "meeting.mp3", nil)
var failure *hushscript.Error
switch {
case err == nil:
	fmt.Println(transcript.Text(nil))
case errors.As(err, &failure) && failure.Kind == hushscript.KindInsufficientBalance:
	fmt.Println(failure.Hint)
case errors.As(err, &failure):
	fmt.Println(failure.Code, failure.Hint)
default:
	return err
}
```

An error carries `Code` (a slug such as `insufficient_balance`), `NumericCode`, `Status` (0 when there
was no response), `Message`, `Hint` (what to do next, or empty), `Details` (the API's own snake_case
keys) and `RetryAfter`. The kinds, with `Kind.String()` giving the class name every Hushscript SDK
uses:

```
ApiError                         has Status (Kind.IsAPI)
├─ AuthenticationError           401
├─ PermissionDeniedError         403
├─ InsufficientBalanceError      402
├─ NotFoundError                 404
├─ ConflictError                 409
├─ ValidationError               400, 422
└─ RateLimitError                429
ConfigurationError
HushscriptError                  unexpected_error, Status 0 (KindUnexpected)
PreflightError                   no credit was held (Kind.IsPreflight)
├─ UnsupportedMediaError
├─ FileLimitError
├─ DailyLimitError
└─ ResumeMismatchError
PaymentMethodRequiredError
UploadError
JobFailedError
WaitTimeoutError                 has ResumeToken
AbortError                       has ResumeToken once a job exists
NetworkError
OutcomeUnknownError
```

An error returned after an upload started, or after a purchase was sent, carries `IdempotencyKey`;
once the job exists it carries `ResumeToken` too. Pass the token to `client.Resume` to carry on
without a second charge.

A failure of the SDK's own after a request was sent, one that fits none of the kinds above, is a
`KindUnexpected` error (class name `HushscriptError`) with the code `unexpected_error` and `Status`
0; the original rides along as its cause, redacted. It is not an API answer, so `Kind.IsAPI` is false
for it. `OnProgress` and `OnResumeToken` cannot return an error: a panic in one is yours, and leaves
the call with the value you raised, never recovered or wrapped. `runtime.Goexit` inside a callback is
unsupported. The SDK does not recover panics from its own internals either; number parsing is fuzzed
instead, so a malformed answer comes back as an error, not a panic.

## What the SDK does for you

- **Checks before it spends.** It sniffs the format from the file's bytes, probes the duration,
  and refuses a file the server would refuse, or one your daily limit cannot cover, before any
  minutes are held. It does not check your balance: the server decides at create whether the job can
  be paid (an organization pool can pay for it), and its 402 arrives before any part is sent.
- **Keeps retries from charging twice.** Every upload and purchase carries an idempotency key, and
  retries reuse it. A resume token replays the same job instead of starting a new one, and the key
  on a failed purchase does the same for `Buy`.
- **Retries what is safe.** Reads, heartbeats and part uploads retry on network errors, 429 and 5xx
  with jittered backoff, and honor `Retry-After` up to 60 seconds. A purchase retries once on a
  network error, 502 or 503. A claim, a rotation and a signup never retry.
- **Paces uploads.** The API allows four upload creates a minute. A loop over a folder waits as
  needed, and `OnProgress` keeps reporting.
- **Keeps the upload alive.** It uploads parts in parallel with bounded memory, falls back to the
  proxied route when direct upload fails, refreshes expired part URLs and sends a heartbeat every two
  minutes.
- **Waits and fetches.** It polls the job until it is done, returns a `KindJobFailed` error if it
  fails, and gives you a resume token if the wait times out. The job keeps running.
- **Reads the token per attempt.** After `RotateToken`, every later attempt, retries already queued
  included, sends the new token.
- **Redacts the token** from every error, log line and formatted `Client`, and the secrets in the signup,
  claim and rotation results from their printed forms.

## Supported formats

MP3, M4A, MP4 and MOV (the audio track of a video is used), WAV, FLAC, and OGG with Vorbis or Opus.
Files can be up to 5 GB and from 15 seconds to 10 hours. The SDK does not convert media. For
anything else, convert it first:

```sh
ffmpeg -i input -vn -c:a libopus output.opus
```

## Configuration

`New` takes functional options:

| Option           | Env var               | Default                                                                      |
| ---------------- | --------------------- | ---------------------------------------------------------------------------- |
| `WithToken`      | `HUSHSCRIPT_TOKEN`    | none                                                                         |
| `WithBaseURL`    | `HUSHSCRIPT_BASE_URL` | `https://api.hushscript.com`                                                 |
| `WithTimeout`    | none                  | 60 s per request attempt                                                     |
| `WithMaxRetries` | none                  | 3 (0 turns retries off)                                                      |
| `WithLogLevel`   | `HUSHSCRIPT_LOG`      | `off` (`debug` logs one metadata line per request attempt to standard error) |
| `WithLogger`     | none                  | none; an `*slog.Logger` that receives the debug lines instead                |
| `WithHTTPClient` | none                  | a plain `http.Client` that never follows redirects                           |
| `WithClock`      | none                  | the system clock; pass a `Clock` in tests                                    |

An explicit option beats its environment variable. The base URL must be `https://`, or `http://` to
`localhost`, `127.0.0.1` or `[::1]`, with no path. Each upload part has its own 300 s timeout, which
you cannot change. `WithHTTPClient` takes a shallow copy of the `*http.Client` at `New`, so
reassigning its fields afterwards does not reach the SDK.

## Privacy

The SDK sends no telemetry. The only client identification is the `User-Agent` header
(`hushscript-go/<version> go/<version>`). Logging is off by default; `HUSHSCRIPT_LOG=debug` (or
`WithLogLevel("debug")`, or `WithLogger`) writes one line per request attempt with the method, path,
status and timing, and never a header, body, title or transcript text.

## The low-level API

Every `/v1` operation is available as `client.API()`, the generated client, sent through the same
retries, timeouts and redaction:

```go
response, err := client.API().GetAccountWithResponse(ctx)
```

`Job.Options` and `UploadLimits` are typed, so a key the API adds to them later is not kept there;
`client.API()` returns the whole body.

## Server only

The SDK is for servers. The API does not accept browser origins, and a token in a browser bundle is a
leaked token.

## Pricing

Personal accounts buy prepaid minute packs. The price of a pack depends on your country, and your
minutes stay valid for a year after your last transcription or purchase. A transcription uses the
minutes of the recording; medical mode and each translation target use more. Current prices are on
[hushscript.com/pricing](https://hushscript.com/pricing).

## Requirements

Go 1.25 or newer.

## Releases

Releases come from release-please. A Go module is published by its tag, so there is no registry
token: when the release PR is merged, the `publish` workflow builds the source archive and a
CycloneDX SBOM, attests the archive's build provenance behind the `release` environment, and attaches
all three to the GitHub release. `proxy.golang.org` is asked for the new version once the tag exists.

## Development

Everything runs in containers; the host needs only `just` and Docker.

```sh
just ci        # lint, then test and conformance on Go 1.25 and 1.27, then examples and build
just test      # unit tests with the race detector and the 85% coverage floor
just examples  # every program in examples/ against the conformance mock
just regen     # regenerate openapi.json and generated/ from the production spec
```

The conformance suite is read at the tag in `conformance-ref`, the one place it is pinned, and no
scenario may be skipped (`CONFORMANCE_STRICT=1`, the default of the justfile). Until the conformance
repository is public, point the mock and the fixtures at a local checkout:
`CONFORMANCE_CONTEXT=../hushscript-sdk-conformance CONFORMANCE_DIR=../hushscript-sdk-conformance just ci`. The unit tests also read the
conformance fixtures (`fixtures/hint-rendering.json`) and fail without them: `just test` and
`just mutation` find a checkout through `CONFORMANCE_DIR`, then a local `CONFORMANCE_CONTEXT`, then
a clone of the pinned tag (`scripts/conformance-dir.sh`).

`generated/` is never edited by hand. `scripts/generate.go` also declares one path parameter the
API document leaves out (`signup_id` on the claim operation), in a private copy of the spec.

### Accepted deviations from the SDK specification

- `fieldalignment` is off in `.golangci.yml`: struct layouts here, `Error` included, are ordered for
  readers, not for padding.
- The Scorecard workflow has `id-token: write`, which section 13.3 allows only for publishing:
  Scorecard needs it to publish its results.
- The `audit` compose service has outbound network access, next to `regen` and `smoke`: `go mod tidy
  -diff` (the module and `tools/`) and `govulncheck` ask the module proxy and the vulnerability database.

## Security

See [SECURITY.md](SECURITY.md).

## License

MIT
