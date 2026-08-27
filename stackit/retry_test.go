package stackit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastRetry is the transport under test with the backoff collapsed, so the tests
// exercise the retry logic without spending the production delays.
func fastRetry() *retryTransport {
	return newRetryTransport(retryAttempts, time.Millisecond)
}

// countingServer serves handler and records how many requests it received.
func countingServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, int(calls.Add(1)))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestRetryTransportRecoversFromBlip covers the 2026-08-25 10:37 incident shape:
// one failed call, everything else healthy. The retry must absorb it so the
// caller never sees an error.
func TestRetryTransportRecoversFromBlip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(w http.ResponseWriter, r *http.Request)
		wantN int
	}{
		{"500 then success", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, 2},
		{"503 then success", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}, 2},
		{"429 then success", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}, 2},
		{"dropped connection then success", func(w http.ResponseWriter, _ *http.Request) {
			// Hijack and close without a response: the client sees an
			// unexpected EOF, the shape reported as "ensure bucket: unexpected EOF".
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := countingServer(t, func(w http.ResponseWriter, r *http.Request, n int) {
				if n == 1 {
					tc.fail(w, r)
					return
				}
				w.WriteHeader(http.StatusOK)
			})

			client := &http.Client{Transport: fastRetry()}
			resp, err := client.Get(srv.URL)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
			if got := int(calls.Load()); got != tc.wantN {
				t.Errorf("server saw %d requests, want %d", got, tc.wantN)
			}
		})
	}
}

// TestRetryTransportGivesUp verifies the attempt budget is finite and the last
// response is handed back rather than swallowed.
func TestRetryTransportGivesUp(t *testing.T) {
	srv, calls := countingServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusBadGateway)
	})

	client := &http.Client{Transport: fastRetry()}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if got := int(calls.Load()); got != retryAttempts {
		t.Errorf("server saw %d requests, want %d", got, retryAttempts)
	}
}

// TestRetryTransportDoesNotRetryDefiniteAnswers pins that the API's own answers
// are returned untouched. Retrying a 403 would multiply the very error storm
// this change exists to prevent.
func TestRetryTransportDoesNotRetryDefiniteAnswers(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, calls := countingServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(status)
			})

			client := &http.Client{Transport: fastRetry()}
			resp, err := client.Get(srv.URL)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != status {
				t.Errorf("status = %d, want %d", resp.StatusCode, status)
			}
			if got := int(calls.Load()); got != 1 {
				t.Errorf("server saw %d requests, want exactly 1", got)
			}
		})
	}
}

// TestRetryTransportLeavesWritesAlone is the safety property: a write may have
// been applied before the connection broke, and repeating CreateAccessKey would
// mint a second key whose secret is unrecoverable. Writes get exactly one try.
func TestRetryTransportLeavesWritesAlone(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			srv, calls := countingServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.WriteHeader(http.StatusServiceUnavailable)
			})

			req, err := http.NewRequest(method, srv.URL, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			client := &http.Client{Transport: fastRetry()}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if got := int(calls.Load()); got != 1 {
				t.Errorf("%s was retried: server saw %d requests, want exactly 1", method, got)
			}
		})
	}
}

// TestRetryTransportHonoursContext makes sure a cancelled reconcile aborts the
// backoff instead of holding a worker for the full budget.
func TestRetryTransportHonoursContext(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}

	// A long backoff makes the test fail loudly if cancellation is ignored.
	client := &http.Client{Transport: newRetryTransport(retryAttempts, 30*time.Second)}

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	resp, err := client.Do(req) //nolint:bodyclose // no response is expected on the cancelled path
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected an error after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancellation took %v; backoff did not abort", elapsed)
	}
}

// TestNewRetryTransportDefaults covers the guard rails on construction.
func TestNewRetryTransportDefaults(t *testing.T) {
	rt := newRetryTransport(0, time.Second)
	if rt.base != http.DefaultTransport {
		t.Error("base must be http.DefaultTransport")
	}
	if rt.attempts != 1 {
		t.Errorf("attempts = %d, want a floor of 1", rt.attempts)
	}
}

// TestRetryingHTTPClientHasNoTimeout pins the deliberate choice not to introduce
// a client-level deadline: the SDK's own default client has none, and adding one
// would silently cap calls that work today.
func TestRetryingHTTPClientHasNoTimeout(t *testing.T) {
	c := retryingHTTPClient()
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (context governs deadlines)", c.Timeout)
	}
	if _, ok := c.Transport.(*retryTransport); !ok {
		t.Errorf("Transport = %T, want *retryTransport", c.Transport)
	}
}
