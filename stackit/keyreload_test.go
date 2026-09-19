package stackit

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guided-traffic/stackit-s3-provisioner/internal/stackitfake"
)

// saKeyOption customises a generated service-account key file.
type saKeyOption func(doc map[string]any)

// otherProject is a project the operator is not bound to.
const otherProject = "99999999-9999-9999-9999-999999999999"

// withOtherProject makes the key name a different project — the candidate the
// project guard has to refuse.
func withOtherProject() saKeyOption {
	return func(doc map[string]any) { doc["projectId"] = otherProject }
}

// withTokenEndpoint points the key flow at a local token server. The SDK reads
// credentials.tokenEndpoint whenever no explicit token URL is configured, which
// is what makes the whole key flow — signing an assertion, exchanging it for an
// access token — exercisable offline.
func withTokenEndpoint(url string) saKeyOption {
	return func(doc map[string]any) {
		doc["credentials"].(map[string]any)["tokenEndpoint"] = url
	}
}

// withValidUntil stamps the expiry the provider sets on a real key. Both e2e
// key files carry none, so the absent case is the one the suites otherwise
// exercise and this is the only way to reach the other.
func withValidUntil(t time.Time) saKeyOption {
	return func(doc map[string]any) { doc["validUntil"] = t.UTC().Format(time.RFC3339) }
}

// withBrokenPrivateKey replaces the PEM with something the SDK cannot parse,
// standing in for a truncated write.
func withBrokenPrivateKey() saKeyOption {
	return func(doc map[string]any) {
		doc["credentials"].(map[string]any)["privateKey"] = "-----BEGIN RSA PRIVATE KEY-----\ntruncated\n"
	}
}

// buildSAKey generates a throwaway RSA key and renders a structurally valid
// STACKIT service-account key document.
func buildSAKey(t *testing.T, opts ...saKeyOption) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	doc := map[string]any{
		"id":           "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"projectId":    fakeProject,
		"active":       true,
		"createdAt":    "2026-01-01T00:00:00Z",
		"keyAlgorithm": "RSA_2048",
		"keyOrigin":    "GENERATED",
		"keyType":      "USER_MANAGED",
		"publicKey":    "",
		"credentials": map[string]any{
			"aud":        "https://stackit-service-account-prod.apps.01.cf.eu01.stackit.cloud",
			"iss":        "test-sa@sa.stackit.cloud",
			"kid":        "11111111-2222-3333-4444-555555555555",
			"sub":        "99999999-8888-7777-6666-555555555555",
			"privateKey": string(keyPEM),
		},
	}
	for _, opt := range opts {
		opt(doc)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal key doc: %v", err)
	}
	return raw
}

// keyFile is a service-account key file a test can rewrite in place, the way an
// external rotation mechanism does.
type keyFile struct {
	t    *testing.T
	path string
}

func newKeyFile(t *testing.T, content []byte) *keyFile {
	t.Helper()
	f := &keyFile{t: t, path: filepath.Join(t.TempDir(), "sa-key.json")}
	f.write(content)
	return f
}

func (f *keyFile) read(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	return raw
}

func (f *keyFile) write(content []byte) {
	f.t.Helper()
	if err := os.WriteFile(f.path, content, 0o600); err != nil {
		f.t.Fatalf("write key file: %v", err)
	}
}

// tokenServer stands in for the STACKIT token endpoint. Its answer is what
// separates a key the provider accepts from one it has revoked, which is the
// distinction the whole validation exists to make.
type tokenServer struct {
	srv *httptest.Server
	mu  sync.Mutex
	// answer renders the response for the next request.
	answer func(w http.ResponseWriter)
	calls  int
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	ts := &tokenServer{}
	ts.grant(t)
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ts.mu.Lock()
		answer, calls := ts.answer, ts.calls+1
		ts.calls = calls
		ts.mu.Unlock()
		answer(w)
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) url() string { return ts.srv.URL }

func (ts *tokenServer) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.calls
}

func (ts *tokenServer) set(answer func(w http.ResponseWriter)) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.answer = answer
}

// grant answers the way the real endpoint does for a key it accepts.
func (ts *tokenServer) grant(t *testing.T) {
	token := fakeAccessToken(t, time.Now().Add(time.Hour))
	ts.set(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600}`, token)
	})
}

// refuse answers the way the real endpoint does for a revoked or deleted key:
// a structured 400 invalid_grant, measured against the live endpoint and
// recorded in the provider-error classification.
func (ts *tokenServer) refuse(status int) {
	ts.set(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	})
}

// gatewayPage answers the way an intermediary in front of the endpoint does:
// the right status code carried by an HTML body, which is not the provider
// deciding anything.
func (ts *tokenServer) gatewayPage(status int) {
	ts.set(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		fmt.Fprint(w, "<html><body>Forbidden</body></html>")
	})
}

// fakeAccessToken builds a JWT the SDK will accept as an access token. The SDK
// only ever parses it unverified to read the expiry, so the signature is
// deliberately not a real one — signing here would prove nothing and would pull
// a JWT library into this package.
func fakeAccessToken(t *testing.T, exp time.Time) string {
	t.Helper()
	part := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal token part: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return strings.Join([]string{
		part(map[string]any{"alg": "RS512", "typ": "JWT"}),
		part(map[string]any{"exp": exp.Unix()}),
		base64.RawURLEncoding.EncodeToString([]byte("offline-signature")),
	}, ".")
}

// reloadEnv is a client pointed at the in-memory fake, a token endpoint under
// the test's control, and a key file the test can rewrite.
type reloadEnv struct {
	client *Client
	fake   *stackitfake.Server
	token  *tokenServer
	file   *keyFile
	loader *KeyReloader
}

// newReloadEnv wires a client whose candidate clients reach the fake rather
// than the real API. NewClientWithEndpoint is what makes that work: it
// remembers the endpoint on the Client, and a reload builds its candidate
// against the same one.
func newReloadEnv(t *testing.T, opts ...saKeyOption) *reloadEnv {
	t.Helper()
	fake := stackitfake.New(fakeProject, fakeRegion)
	t.Cleanup(fake.Close)
	token := newTokenServer(t)
	client, err := NewClientWithEndpoint(fakeProject, fakeRegion, fake.CP.URL)
	if err != nil {
		t.Fatalf("NewClientWithEndpoint: %v", err)
	}
	all := append([]saKeyOption{withTokenEndpoint(token.url())}, opts...)
	env := &reloadEnv{
		client: client,
		fake:   fake,
		token:  token,
		file:   newKeyFile(t, buildSAKey(t, all...)),
	}
	// tick is driven directly, so the observer is not the subject here; the
	// poll loop that does call it has its own test.
	env.loader = NewKeyReloader(client, env.file.path, 30*time.Second, nil)
	return env
}

// rewrite replaces the key file with a freshly generated key, always keeping
// the token endpoint so the candidate stays offline.
func (e *reloadEnv) rewrite(t *testing.T, opts ...saKeyOption) {
	t.Helper()
	e.file.write(buildSAKey(t, append([]saKeyOption{withTokenEndpoint(e.token.url())}, opts...)...))
}

// tick runs one poll and returns what it reported.
func (e *reloadEnv) tick(t *testing.T) (ReloadResult, bool) {
	t.Helper()
	return e.loader.tick(context.Background())
}

// mustApply runs one poll and requires that the candidate was accepted.
func (e *reloadEnv) mustApply(t *testing.T) ReloadResult {
	t.Helper()
	res, reported := e.tick(t)
	if !reported {
		t.Fatalf("poll reported nothing, want an applied reload")
	}
	if res.Outcome != ReloadApplied {
		t.Fatalf("outcome = %q (err %v), want %q", res.Outcome, res.Err, ReloadApplied)
	}
	return res
}

func TestReloadAcceptsAProvenKey(t *testing.T) {
	env := newReloadEnv(t, withValidUntil(time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)))
	before := env.client.keyHash()

	res := env.mustApply(t)

	if res.Hash == before || res.Hash == "" {
		t.Errorf("hash = %q, want a new non-empty hash (was %q)", res.Hash, before)
	}
	if env.client.keyHash() != res.Hash {
		t.Errorf("client hash = %q, want %q", env.client.keyHash(), res.Hash)
	}
	acc := env.client.Account()
	if acc.ProjectID != fakeProject {
		t.Errorf("ProjectID = %q, want %q", acc.ProjectID, fakeProject)
	}
	if acc.Issuer != "test-sa@sa.stackit.cloud" {
		t.Errorf("Issuer = %q, want the key's iss", acc.Issuer)
	}
	if acc.ValidUntil == nil || !acc.ValidUntil.Equal(time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("ValidUntil = %v, want 2027-03-01T12:00:00Z", acc.ValidUntil)
	}
	// The proof is a real authenticated call: a token was minted and the
	// control plane was asked something with it.
	if env.token.count() != 1 {
		t.Errorf("token requests = %d, want 1", env.token.count())
	}
	if got := env.fake.Calls("ServiceStatus"); got != 1 {
		t.Errorf("ServiceStatus calls = %d, want 1", got)
	}
}

func TestReloadUnchangedContentIsANoOp(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)
	probesAfterApply := env.fake.Calls("ServiceStatus")

	res, reported := env.tick(t)

	if !reported || res.Outcome != ReloadUnchanged {
		t.Fatalf("outcome = %q (reported %v), want %q", res.Outcome, reported, ReloadUnchanged)
	}
	if got := env.fake.Calls("ServiceStatus"); got != probesAfterApply {
		t.Errorf("ServiceStatus calls = %d, want %d — an unchanged file must cost no API call", got, probesAfterApply)
	}
	// The second identical poll is the one that must stop being logged.
	if again, _ := env.tick(t); !again.Repeated {
		t.Errorf("Repeated = false on the second unchanged poll, want true")
	}
}

// TestReloadRejectsWithoutDisplacingTheRunningKey is the core safety property:
// every way a candidate can be wrong leaves the operator exactly where it was.
func TestReloadRejectsWithoutDisplacingTheRunningKey(t *testing.T) {
	cases := []struct {
		name       string
		corrupt    func(t *testing.T, env *reloadEnv)
		definitive bool
	}{
		{
			name:       "empty file",
			corrupt:    func(_ *testing.T, env *reloadEnv) { env.file.write(nil) },
			definitive: true,
		},
		{
			name:       "not JSON",
			corrupt:    func(_ *testing.T, env *reloadEnv) { env.file.write([]byte("{\"projectId\": ")) },
			definitive: true,
		},
		{
			name:       "no projectId",
			corrupt:    func(_ *testing.T, env *reloadEnv) { env.file.write([]byte(`{"credentials":{"iss":"x"}}`)) },
			definitive: true,
		},
		{
			name: "foreign project",
			corrupt: func(t *testing.T, env *reloadEnv) {
				env.rewrite(t, withOtherProject())
			},
			definitive: true,
		},
		{
			name:       "unusable key material",
			corrupt:    func(t *testing.T, env *reloadEnv) { env.rewrite(t, withBrokenPrivateKey()) },
			definitive: true,
		},
		{
			name: "provider refuses the key",
			corrupt: func(t *testing.T, env *reloadEnv) {
				env.rewrite(t)
				env.token.refuse(http.StatusBadRequest)
			},
			definitive: true,
		},
		{
			name: "token endpoint is broken",
			corrupt: func(t *testing.T, env *reloadEnv) {
				env.rewrite(t)
				env.token.refuse(http.StatusInternalServerError)
			},
			definitive: false,
		},
		{
			name: "a gateway page, not the provider",
			corrupt: func(t *testing.T, env *reloadEnv) {
				env.rewrite(t)
				env.token.gatewayPage(http.StatusForbidden)
			},
			definitive: false,
		},
		{
			name: "the API refuses the probe",
			corrupt: func(t *testing.T, env *reloadEnv) {
				env.rewrite(t)
				env.fake.FailNext("ServiceStatus", http.StatusForbidden)
			},
			definitive: true,
		},
		{
			name: "the API is unreachable",
			corrupt: func(t *testing.T, env *reloadEnv) {
				env.rewrite(t)
				env.fake.Close()
			},
			definitive: false,
		},
		{
			name:       "the file is gone",
			corrupt:    func(_ *testing.T, env *reloadEnv) { os.Remove(env.file.path) },
			definitive: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReloadEnv(t)
			// Start from a key that was proven, so there is something to lose.
			env.mustApply(t)
			live := env.client.keyHash()
			liveAccount := env.client.Account()

			tc.corrupt(t, env)
			res, reported := env.tick(t)

			if !reported || res.Outcome != ReloadRejected {
				t.Fatalf("outcome = %q (reported %v, err %v), want %q", res.Outcome, reported, res.Err, ReloadRejected)
			}
			if res.Err == nil {
				t.Errorf("Err = nil, want the reason for the rejection")
			}
			if res.Definitive != tc.definitive {
				t.Errorf("Definitive = %v, want %v (err: %v)", res.Definitive, tc.definitive, res.Err)
			}
			if env.client.keyHash() != live {
				t.Errorf("client hash = %q, want the running key %q to be untouched", env.client.keyHash(), live)
			}
			if env.client.Account().ProjectID != liveAccount.ProjectID {
				t.Errorf("client project = %q, want %q", env.client.Account().ProjectID, liveAccount.ProjectID)
			}
		})
	}
}

func TestReloadForeignProjectNamesBothProjects(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)
	env.rewrite(t, withOtherProject())

	res, _ := env.tick(t)

	if res.Err == nil || !strings.Contains(res.Err.Error(), otherProject) || !strings.Contains(res.Err.Error(), fakeProject) {
		t.Errorf("Err = %v, want it to name both the candidate's project and the operator's", res.Err)
	}
}

// TestReloadRecoversAfterTheKeyIsFixed pins the point of the retry policy: a
// rejection is never terminal.
func TestReloadRecoversAfterTheKeyIsFixed(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)
	env.rewrite(t, withOtherProject())
	if res, _ := env.tick(t); res.Outcome != ReloadRejected {
		t.Fatalf("outcome = %q, want a rejection to set the scene", res.Outcome)
	}

	// A different content resets the schedule, so the very next poll retries.
	env.rewrite(t)
	res, reported := env.tick(t)

	if !reported || res.Outcome != ReloadApplied {
		t.Fatalf("outcome = %q (reported %v, err %v), want %q", res.Outcome, reported, res.Err, ReloadApplied)
	}
}

// TestReloadBacksOffOnARepeatedDefinitiveRejection pins the two schedules of
// the retry policy: a definitive rejection doubles its wait, a non-definitive
// one retries every poll.
func TestReloadBacksOffOnARepeatedDefinitiveRejection(t *testing.T) {
	t.Run("definitive rejections double the wait", func(t *testing.T) {
		env := newReloadEnv(t)
		env.mustApply(t)
		env.rewrite(t, withOtherProject())

		// Polls at which an attempt is actually made, counting from the first.
		// One interval, then two, then four, then eight.
		var attempts []int
		for poll := 0; poll < 16; poll++ {
			if _, reported := env.tick(t); reported {
				attempts = append(attempts, poll)
			}
		}
		want := []int{0, 1, 3, 7, 15}
		if fmt.Sprint(attempts) != fmt.Sprint(want) {
			t.Errorf("attempted at polls %v, want %v", attempts, want)
		}
	})

	t.Run("a cap bounds the wait", func(t *testing.T) {
		// With a one-minute interval the ten-minute cap is ten polls.
		r := NewKeyReloader(nil, "", time.Minute, nil)
		if got := r.maxWaitIntervals(); got != 10 {
			t.Errorf("maxWaitIntervals = %d, want 10", got)
		}
		// An interval longer than the cap still retries every poll.
		r = NewKeyReloader(nil, "", time.Hour, nil)
		if got := r.maxWaitIntervals(); got != 1 {
			t.Errorf("maxWaitIntervals = %d, want 1", got)
		}
	})

	t.Run("a non-definitive rejection retries every poll", func(t *testing.T) {
		env := newReloadEnv(t)
		env.mustApply(t)
		env.rewrite(t)
		env.token.refuse(http.StatusInternalServerError)

		for poll := 0; poll < 3; poll++ {
			res, reported := env.tick(t)
			if !reported || res.Outcome != ReloadRejected || res.Definitive {
				t.Fatalf("poll %d: outcome %q, reported %v, definitive %v — want a reported, non-definitive rejection",
					poll, res.Outcome, reported, res.Definitive)
			}
		}
	})
}

// TestReloadRejectionIsLoudOnceThenQuiet pins what keeps the log readable
// without hiding a new problem.
func TestReloadRejectionIsLoudOnceThenQuiet(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)
	env.rewrite(t)
	env.token.refuse(http.StatusInternalServerError)

	first, _ := env.tick(t)
	second, _ := env.tick(t)
	if first.Repeated {
		t.Errorf("first rejection Repeated = true, want false")
	}
	if !second.Repeated {
		t.Errorf("second rejection of the same content Repeated = false, want true")
	}

	// Different content is a different problem and must be loud again.
	env.rewrite(t)
	third, _ := env.tick(t)
	if third.Repeated {
		t.Errorf("rejection of new content Repeated = true, want false")
	}
}

// TestReloadSwapIsAtomicUnderConcurrentReaders checks that readers never see a
// half-swapped client. Note that the repository's test targets do not run with
// -race, so this is a structural check plus a smoke test; run it with
// `go test -race -run Atomic ./stackit/...` to get the real guarantee.
func TestReloadSwapIsAtomicUnderConcurrentReaders(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)

	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for ctx.Err() == nil {
				if got := env.client.ProjectID(); got != fakeProject {
					t.Errorf("ProjectID() = %q mid-swap, want %q", got, fakeProject)
					return
				}
				if env.client.Account().Issuer == "" {
					t.Errorf("Account() came back half-built mid-swap")
					return
				}
			}
		}()
	}

	for i := 0; i < 20; i++ {
		env.rewrite(t)
		if res, _ := env.tick(t); res.Outcome != ReloadApplied {
			cancel()
			readers.Wait()
			t.Fatalf("swap %d: outcome %q, err %v", i, res.Outcome, res.Err)
		}
	}
	cancel()
	readers.Wait()
}

func TestKeyReloaderNeedsNoLeaderElection(t *testing.T) {
	// A standby has to hold a fresh client at the moment it takes the lease,
	// not start reloading then.
	if NewKeyReloader(nil, "", time.Second, nil).NeedLeaderElection() {
		t.Errorf("NeedLeaderElection() = true, want false")
	}
}

func TestKeyReloaderStartStopsOnContextCancel(t *testing.T) {
	cases := map[string]time.Duration{
		"polling":  10 * time.Millisecond,
		"disabled": 0,
	}
	for name, interval := range cases {
		t.Run(name, func(t *testing.T) {
			env := newReloadEnv(t)
			r := NewKeyReloader(env.client, env.file.path, interval, nil)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Start(ctx) }()
			cancel()
			select {
			case err := <-done:
				// A non-nil error from a Runnable tears the whole manager down.
				if err != nil {
					t.Errorf("Start returned %v, want nil on shutdown", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("Start did not return after the context was cancelled")
			}
		})
	}
}

func TestKeyReloaderStartAppliesAChangedKey(t *testing.T) {
	env := newReloadEnv(t)
	applied := make(chan ReloadResult, 8)
	r := NewKeyReloader(env.client, env.file.path, 10*time.Millisecond, func(res ReloadResult) {
		if res.Outcome == ReloadApplied {
			select {
			case applied <- res:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = r.Start(ctx) }()

	select {
	case res := <-applied:
		if env.client.keyHash() != res.Hash {
			t.Errorf("client hash = %q, want %q", env.client.keyHash(), res.Hash)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the poller never applied the key on disk")
	}
}

// TestReloadNoticesACorrectionWhileBackingOff is the regression test for the
// gap the backoff used to leave: the schedule is allowed to throttle provider
// calls, but never the look at the disk. Without it, an operator who fixed the
// key file waited out the remaining backoff — at the cap, ten minutes after the
// working key was already in place.
func TestReloadNoticesACorrectionWhileBackingOff(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)

	// Two definitive rejections of the same content, so the schedule has grown
	// past "retry on the very next poll".
	env.rewrite(t, withOtherProject())
	for poll := 0; poll < 2; poll++ {
		if res, reported := env.tick(t); !reported || res.Outcome != ReloadRejected {
			t.Fatalf("poll %d: outcome %q (reported %v), want a rejection", poll, res.Outcome, reported)
		}
	}
	if env.loader.ticksLeft == 0 {
		t.Fatalf("ticksLeft = 0, want the schedule to be skipping polls by now")
	}

	// The operator fixes the file. The very next poll must act on it.
	env.rewrite(t)
	res, reported := env.tick(t)

	if !reported || res.Outcome != ReloadApplied {
		t.Fatalf("outcome = %q (reported %v, err %v), want the corrected key to be applied at once",
			res.Outcome, reported, res.Err)
	}
}

// TestReloadRevertingTheFileEndsTheRejectionAtOnce is the same property for the
// other correction an operator makes: putting back the key that was working.
func TestReloadRevertingTheFileEndsTheRejectionAtOnce(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)
	good := env.file.read(t)

	env.rewrite(t, withOtherProject())
	for poll := 0; poll < 2; poll++ {
		env.tick(t)
	}
	if env.loader.ticksLeft == 0 {
		t.Fatalf("ticksLeft = 0, want the schedule to be skipping polls by now")
	}

	env.file.write(good)
	res, reported := env.tick(t)

	if !reported || res.Outcome != ReloadUnchanged {
		t.Fatalf("outcome = %q (reported %v), want %q so the failing gauge clears at once",
			res.Outcome, reported, ReloadUnchanged)
	}
}

// TestReloadTransientFailureKeepsTheBackoffItHasEarned pins the cap as an
// actual bound. A poll that did not reach the provider says nothing about the
// candidate, so it must not hand a permanently bad key a fresh 1-2-4-8
// schedule — otherwise a flapping provider turns "one call every ten minutes"
// into one call per poll, forever.
func TestReloadTransientFailureKeepsTheBackoffItHasEarned(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)

	// Grow the schedule with definitive rejections of one candidate. The
	// provider refusing the key is the definitive class that still reaches the
	// token endpoint, which is what lets the same content fail both ways.
	env.rewrite(t)
	env.token.refuse(http.StatusBadRequest)
	for poll := 0; poll < 8; poll++ {
		env.tick(t)
	}
	grown := env.loader.waitIntervals
	if grown < 4 {
		t.Fatalf("waitIntervals = %d after eight polls, want a grown schedule", grown)
	}

	// The same content, now failing in a way that says nothing about it:
	// retried at once, but the schedule it earned survives.
	env.token.refuse(http.StatusInternalServerError)
	env.loader.ticksLeft = 0
	res, reported := env.tick(t)
	if !reported || res.Definitive {
		t.Fatalf("outcome %q definitive %v, want a reported non-definitive rejection", res.Outcome, res.Definitive)
	}
	if env.loader.waitIntervals != grown {
		t.Errorf("waitIntervals = %d after a transient failure, want it to stay at %d",
			env.loader.waitIntervals, grown)
	}
	if env.loader.ticksLeft != 0 {
		t.Errorf("ticksLeft = %d, want 0 — a non-definitive rejection retries at once", env.loader.ticksLeft)
	}
}

// TestReloadReturnsOnShutdownRatherThanWaitingOutTheTokenFetch pins the reason
// the probe runs on its own goroutine. The SDK builds its token request without
// a context, so a token endpoint that accepts the connection and never answers
// is uninterruptible for the SDK's own minute. Waiting for it would hold the
// manager past its graceful-shutdown period and turn a rolling update into a
// non-zero exit.
func TestReloadReturnsOnShutdownRatherThanWaitingOutTheTokenFetch(t *testing.T) {
	env := newReloadEnv(t)
	env.mustApply(t)

	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	env.token.set(func(w http.ResponseWriter) { <-hang })
	env.rewrite(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.loader.tick(ctx)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the poll did not return on cancellation; it is waiting out the token fetch")
	}
}
