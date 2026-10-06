// Package hushscript is the official Go SDK for the Hushscript transcription API.
//
// New builds a Client from options and the HUSHSCRIPT_TOKEN, HUSHSCRIPT_BASE_URL and HUSHSCRIPT_LOG
// environment variables. Client.API returns the generated client for every /v1 operation, wired to
// the SDK's retrying HTTP core: timeouts, retries with backoff, Retry-After handling, upload-create
// pacing, token redaction and typed errors. A failed call returns a *Error; use errors.As to read
// its Kind, Code, Status, Hint and Details.
//
// The SDK never logs headers, query strings, bodies or transcript text, and never prints the token.
package hushscript
