# Hushscript Go SDK

The official Go SDK for the Hushscript API. It is a work in progress for v0.1: this repository
currently holds the generated low-level client and the hardened HTTP core.

```go
import hushscript "github.com/Hushscript/hushscript-go"

client, err := hushscript.New() // reads HUSHSCRIPT_TOKEN
if err != nil {
	log.Fatal(err)
}
api := client.API() // the generated client, wired to the retrying HTTP core
```

Requires Go 1.25 or newer. Server-side use only: a token must never ship in a browser bundle.

## Development

Everything runs in containers; the host needs only `just` and Docker.

```sh
just ci        # lint, then test and conformance on Go 1.25 and 1.26, then examples and build
just test      # unit tests with the race detector and the 85% coverage floor
just regen     # regenerate openapi.json and generated/ from the production spec
```

Until the conformance repository is public, point the mock at a local checkout:
`CONFORMANCE_CONTEXT=../hushscript-sdk-conformance just ci`.

`generated/` is never edited by hand. `scripts/generate.go` also declares one path parameter the
API document leaves out (`signup_id` on the claim operation), in a private copy of the spec.
