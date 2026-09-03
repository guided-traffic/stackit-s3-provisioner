package stackit

import (
	"io"
	"net/http"
	"time"
)

const (
	// retryAttempts is the total number of tries (1 initial + 2 retries) for a
	// safely repeatable control-plane request.
	retryAttempts = 3
	// retryBackoff is the delay before the first retry; it triples for the next
	// one. Total added latency in the worst case stays well under a second, so a
	// retrying request never outlives a reconcile.
	retryBackoff = 200 * time.Millisecond
	// idleConnTimeout is how long a pooled keep-alive connection may sit unused
	// before this client closes it. It is deliberately far below
	// http.DefaultTransport's 90s.
	//
	// The operator talks to the API in bursts (one per drift resync) and is then
	// idle for minutes, so every burst after the first reuses a connection the
	// provider's edge may already have dropped. That surfaces as
	// "read: connection reset by peer" on a GET, which retryTransport then
	// re-runs against the next equally stale connection in the pool. Closing
	// first turns those into a fresh dial instead of an error.
	//
	// Not verified: the edge's own idle timeout. 30s is chosen to sit below any
	// plausible value rather than to match a measured one.
	idleConnTimeout = 30 * time.Second
)

// retryTransport retries a control-plane request whose failure carries no
// information about the request itself: a transport error (connection reset,
// unexpected EOF, timeout) or a 5xx from the server. It exists because the
// STACKIT SDK does not retry — config.WithMaxRetries has been a no-op since
// core v0.26.0 — so a single dropped connection surfaces as a failed reconcile.
//
// Only GET and HEAD are retried. A failed write is ambiguous: the server may
// have applied it before the connection broke, and repeating CreateAccessKey
// would mint a second key whose secret is returned once and then lost, leaking
// a credential that nothing tracks. Read failures dominate the hot path anyway
// (GetServiceStatus, GetBucket, ListBuckets, ListGroups, ListKeys), so the safe
// subset is also the useful one.
//
// The same exclusion applies to the key flow's token fetch, which is a POST: it
// passes through this transport unretried. See NewClient for why that is left
// alone rather than special-cased.
//
// The S3 data plane is not covered here and does not need to be: minio-go
// retries retryable failures on its own.
type retryTransport struct {
	base     http.RoundTripper
	attempts int
	backoff  time.Duration
}

// newRetryTransport builds the transport on top of a clone of
// http.DefaultTransport, keeping the connection pooling the SDK relies on but
// with a shorter idle timeout (see idleConnTimeout). attempts is floored at 1 so
// a misconfiguration degrades to "no retries" rather than to no request at all.
func newRetryTransport(attempts int, backoff time.Duration) *retryTransport {
	if attempts < 1 {
		attempts = 1
	}
	return &retryTransport{base: newBaseTransport(), attempts: attempts, backoff: backoff}
}

// newBaseTransport clones http.DefaultTransport and shortens its idle timeout.
// Cloning rather than mutating the shared default keeps the change local to this
// client; mutating it would reach every other HTTP user in the process.
func newBaseTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Unreachable with the standard library, but a type assertion that can
		// fail must not panic a running operator.
		return &http.Transport{IdleConnTimeout: idleConnTimeout}
	}
	t := base.Clone()
	t.IdleConnTimeout = idleConnTimeout
	return t
}

// RoundTrip implements http.RoundTripper.
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !retryableRequest(req) {
		return t.base.RoundTrip(req)
	}

	ctx := req.Context()
	delay := t.backoff

	var resp *http.Response
	var err error
	for attempt := 1; ; attempt++ {
		resp, err = t.base.RoundTrip(req)
		if attempt >= t.attempts || !retryableResult(resp, err) {
			return resp, err
		}
		// Drain and close the discarded response so the connection can be
		// reused instead of being torn down on every retry.
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
		}
		if !sleepCtx(ctx, delay) {
			// Context cancelled during backoff: report why, not a stale result.
			return nil, ctx.Err()
		}
		delay *= 3
	}
}

// retryableRequest reports whether repeating req is safe. Beyond the read-only
// method check it refuses any request carrying a body: a body is consumed by the
// first attempt, so replaying one would need GetBody bookkeeping for a case the
// SDK never produces. Keeping the guarantee local beats trusting the caller.
func retryableRequest(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return req.Body == nil
}

// retryableResult reports whether the outcome of an attempt says nothing about
// the request and is therefore worth repeating: any transport error, or a
// server-side 5xx. Every 4xx is an answer, and repeating it produces the same
// answer.
//
// 429 is deliberately NOT retried, even though it is transient. It is the one
// status that says "you are sending too much", so repeating the request is the
// only reply guaranteed to make things worse. Verified on mgmt-p 2026-09-02: a
// 503 storm from the provider edge at 14:23 turned into
// "rate limit on IP level exceeded" at 14:34 and escalated to 51 rate-limited
// requests per minute by 14:42, with this transport multiplying every GET by
// three throughout. Backing off from a 429 belongs to the workqueue and the
// provider circuit breaker, which act on the whole fleet rather than on one
// in-flight request.
func retryableResult(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode >= http.StatusInternalServerError
}

// retryingHTTPClient builds the http.Client handed to the SDK. It deliberately
// sets no Timeout, matching the SDK's own default (an http.Client with zero
// value fields): request deadlines stay the caller's context's business, and
// adding one here would silently cap long-running calls that work today.
func retryingHTTPClient() *http.Client {
	return &http.Client{Transport: newRetryTransport(retryAttempts, retryBackoff)}
}
