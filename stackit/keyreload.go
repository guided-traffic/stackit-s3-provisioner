package stackit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	// reloadRejectBackoffMax caps the wait between retries of a candidate the
	// operator has definitively rejected (ADR 0016 D7). The worst case it buys
	// is one token request every ten minutes for a key that will never be
	// accepted — in a state that is already alarming.
	reloadRejectBackoffMax = 10 * time.Minute
	// reloadProbeTimeout bounds the API call a validation makes. It is
	// deliberately ABOVE the SDK's own auth-client timeout of one minute,
	// because the validation has two legs and this context only reaches the
	// second one: the key flow's token POST is built without a context at all,
	// so a budget below a minute can expire *inside* the token fetch and then
	// fail the API call that follows — rejecting a perfectly good key, and
	// rejecting it non-definitively, so it is retried and rejected again
	// forever.
	reloadProbeTimeout = 90 * time.Second
)

// ReloadOutcome is what one poll of the key file did.
type ReloadOutcome string

const (
	// ReloadUnchanged means the file still holds the key already in use.
	ReloadUnchanged ReloadOutcome = "unchanged"
	// ReloadApplied means a candidate passed every check and is now live.
	ReloadApplied ReloadOutcome = "applied"
	// ReloadRejected means a candidate failed a check and was discarded. The
	// key already in use is untouched.
	ReloadRejected ReloadOutcome = "rejected"
)

// ReloadResult reports one poll to whoever is counting and logging. It carries
// no key material and never will: Err is wrapped from the parser, the SDK or
// the provider, none of which echo the candidate's bytes.
type ReloadResult struct {
	Outcome ReloadOutcome
	// Hash identifies the candidate's content. It is empty only when the file
	// could not be read at all, so there was nothing to hash.
	Hash string
	// Account is the service account now in use. Set for ReloadApplied only.
	Account Account
	// Err is why the candidate was rejected, nil otherwise.
	Err error
	// Definitive reports that repeating this same candidate cannot change the
	// outcome — a foreign project, unusable key material, or the provider's own
	// refusal. It decides which of the two retry schedules applies.
	Definitive bool
	// Repeated is true when the previous poll already reported this outcome for
	// this same content. It is what lets a rejection be logged loudly once and
	// stay quiet afterwards, without silencing the next, different rejection.
	Repeated bool
}

// keyHash returns the hash of the key file content the live credential was
// built from, or "" for a client not built from a key file.
func (c *Client) keyHash() string { return c.state.Load().keyHash }

// hashKey is the change detector. A content hash rather than a modification
// time, because a Kubernetes Secret volume is refreshed by an atomic directory
// swap that leaves mtime unhelpful — and because a file changed back to the
// content already loaded is then a no-op for free (ADR 0016 D2).
func hashKey(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// readKeyFile reads the key file once and hashes exactly those bytes. Reading
// once is the point: the same bytes are hashed, parsed and handed to the SDK,
// so no second read of a file that may be mid-rewrite can slip between the
// check and the use (ADR 0016 D3).
func readKeyFile(keyPath string) (raw []byte, hash string, err error) {
	// keyPath is operator-supplied configuration, not attacker-controlled input.
	raw, err = os.ReadFile(keyPath) // #nosec G304
	if err != nil {
		return nil, "", fmt.Errorf("read key file: %w", err)
	}
	return raw, hashKey(raw), nil
}

// definitiveError marks a rejection that repeating the same candidate cannot
// change. It is a wrapper rather than a sentinel because every one of these
// carries a different cause, and the cause is what gets logged.
type definitiveError struct{ err error }

func (e definitiveError) Error() string { return e.err.Error() }
func (e definitiveError) Unwrap() error { return e.err }

func definitive(err error) error { return definitiveError{err: err} }

// isDefinitive reports whether err is a rejection no retry of the same bytes
// can fix.
func isDefinitive(err error) bool {
	var d definitiveError
	return errors.As(err, &d)
}

// reload validates a candidate key and, only if every check passes, replaces
// the client's authenticated state with one built from it.
//
// Nothing is swapped before the last check returns (ADR 0016 D3). A rejection
// is a complete no-op: the running credential, its connection pool and the
// cached service-ready answer are all untouched, so a bad candidate cannot cost
// availability.
func (c *Client) reload(ctx context.Context, raw []byte, hash string, acc Account) error {
	// The project check. One string comparison, no state, no configuration —
	// and a guardrail for this process only, not a boundary: a restart erases
	// the reference point (ADR 0016 D4).
	if acc.ProjectID != c.ProjectID() {
		return definitive(fmt.Errorf("candidate key names project %s, the operator is bound to %s",
			acc.ProjectID, c.ProjectID()))
	}

	// Free validation: the SDK unmarshals the key and parses the RSA PEM
	// eagerly here, so truncated or malformed material fails before any call.
	api, transport, err := c.newKeyFlowAPIClient(raw)
	if err != nil {
		return definitive(fmt.Errorf("build a client from the candidate key: %w", err))
	}
	next := &clientState{api: api, account: acc, transport: transport, keyHash: hash}

	// The only check that proves the key can actually mint a token. Everything
	// above passes for a perfectly well-formed key the provider has revoked.
	//
	// It runs on its own goroutine so that shutdown is never held by it. The
	// token POST inside it carries no context, so cancelling probeCtx cannot
	// interrupt a token fetch that has already started; waiting for one would
	// hold the manager past its graceful-shutdown period and turn a rolling
	// update into a non-zero exit. The abandoned goroutine ends within the
	// SDK's own minute and writes to a buffered channel, so nothing leaks
	// beyond the candidate's connection pool, which the process is leaving
	// anyway.
	probeCtx, cancel := context.WithTimeout(ctx, reloadProbeTimeout)
	defer cancel()
	probed := make(chan error, 1)
	go func() { probed <- c.probe(probeCtx, next) }()

	var probeErr error
	select {
	case probeErr = <-probed:
	case <-ctx.Done():
		return ctx.Err()
	}
	if probeErr != nil {
		transport.CloseIdleConnections()
		if ProviderRefused(probeErr) {
			return definitive(fmt.Errorf("provider refused the candidate key: %w", probeErr))
		}
		return fmt.Errorf("probe the candidate key: %w", probeErr)
	}

	retired := c.state.Swap(next)
	if retired != nil && retired.transport != nil {
		retired.transport.CloseIdleConnections()
	}
	return nil
}

// KeyReloader re-reads the operator's service-account key file and puts a
// changed key to work without a restart, having first proved it (ADR 0016).
//
// It satisfies controller-runtime's Runnable and LeaderElectionRunnable without
// importing either: the manager only needs the two method signatures, and this
// package deliberately depends on nothing above it.
//
// It logs nothing and counts nothing itself. Each poll is handed to observe,
// which is where the operator's metrics and log lines live — that is what keeps
// this package free of a logger and of Prometheus.
type KeyReloader struct {
	client   *Client
	keyPath  string
	interval time.Duration
	observe  func(ReloadResult)

	// Poll-loop state. Touched only from Start's goroutine, so it needs no
	// synchronisation; the swap it drives is what is atomic.

	// lastHash and lastOutcome are what the previous poll saw, and together
	// decide ReloadResult.Repeated.
	lastHash    string
	lastOutcome ReloadOutcome
	// backoffHash is the file content the current backoff was earned by. The
	// schedule is keyed to it rather than to whatever the last poll happened to
	// see, so a transient failure — which says nothing about the candidate —
	// cannot wipe the wait a definitively rejected candidate has accrued and
	// turn the ten-minute cap into no bound at all.
	backoffHash string
	// waitIntervals is how many poll intervals apart the retries of that
	// candidate are. It starts at one and doubles.
	waitIntervals int
	// ticksLeft counts the polls still to be skipped before the next retry.
	ticksLeft int
}

// NewKeyReloader builds the poller. interval must be positive; the caller
// decides whether the feature is on at all, because an interval of zero means
// "restore the behaviour that needed a restart" rather than "poll as fast as
// possible". observe may be nil.
func NewKeyReloader(client *Client, keyPath string, interval time.Duration, observe func(ReloadResult)) *KeyReloader {
	return &KeyReloader{
		client:        client,
		keyPath:       keyPath,
		interval:      interval,
		observe:       observe,
		waitIntervals: 1,
	}
}

// NeedLeaderElection reports false: every replica reloads. A standby has to be
// holding a fresh client at the moment it takes the lease, not start reloading
// then (ADR 0016 D13).
func (r *KeyReloader) NeedLeaderElection() bool { return false }

// Start polls until ctx is cancelled. It returns nil on shutdown — a non-nil
// error from a Runnable tears the whole manager down, and a key that cannot be
// reloaded is not a reason to stop provisioning with the key that works.
func (r *KeyReloader) Start(ctx context.Context) error {
	if r.interval <= 0 {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if res, reported := r.tick(ctx); reported && r.observe != nil {
				r.observe(res)
			}
		}
	}
}

// tick is one poll. It reports false when the poll did nothing at all because
// a definitively rejected candidate is still backing off — that is a deliberate
// silence, not an outcome, and counting it would make the rejection look like
// it was happening far more often than it is.
func (r *KeyReloader) tick(ctx context.Context) (ReloadResult, bool) {
	// The file is read on EVERY poll, backoff or not. The backoff exists to
	// bound provider calls, not to stop looking at the disk, and reading first
	// is what makes "the schedule resets when the content changes" true at the
	// next poll rather than at the next scheduled retry — which at the cap
	// would be ten minutes after somebody already fixed the key.
	raw, hash, err := readKeyFile(r.keyPath)
	if err != nil {
		// No bytes, so no hash to key a backoff on — and a syscall costs
		// nothing, so this is simply retried every poll. The file reappearing
		// is a real recovery path.
		return r.rejected("", err, false), true
	}
	if hash == r.client.keyHash() {
		return r.settled(ReloadUnchanged, hash, Account{}), true
	}
	if hash == r.backoffHash && r.ticksLeft > 0 {
		// The same definitively rejected content is still backing off. Only the
		// validation waits; any other content skips the wait entirely.
		r.ticksLeft--
		return ReloadResult{}, false
	}

	acc, err := parseAccount(raw, r.keyPath)
	if err != nil {
		return r.rejected(hash, definitive(err), true), true
	}
	if err := r.client.reload(ctx, raw, hash, acc); err != nil {
		if ctx.Err() != nil {
			// Shutdown, not a verdict on the candidate. Counting it would leave
			// the last scrape before exit reporting a rejection that never
			// happened.
			return ReloadResult{}, false
		}
		return r.rejected(hash, err, isDefinitive(err)), true
	}
	return r.settled(ReloadApplied, hash, r.client.Account()), true
}

// settled records a poll that left the operator on a working key, and clears
// any backoff.
func (r *KeyReloader) settled(outcome ReloadOutcome, hash string, acc Account) ReloadResult {
	res := ReloadResult{
		Outcome:  outcome,
		Hash:     hash,
		Account:  acc,
		Repeated: r.lastOutcome == outcome && r.lastHash == hash,
	}
	r.lastOutcome, r.lastHash = outcome, hash
	r.waitIntervals, r.ticksLeft, r.backoffHash = 1, 0, ""
	return res
}

// rejected records a discarded candidate and schedules its retry. A definitive
// rejection backs off, doubling from one interval up to the cap; anything else
// is retried every poll, because the provider is not answering and the
// candidate is innocent (ADR 0016 D7).
func (r *KeyReloader) rejected(hash string, err error, isDefinitiveRejection bool) ReloadResult {
	res := ReloadResult{
		Outcome:    ReloadRejected,
		Hash:       hash,
		Err:        err,
		Definitive: isDefinitiveRejection,
		Repeated:   r.lastOutcome == ReloadRejected && r.lastHash == hash,
	}
	if isDefinitiveRejection {
		if hash != r.backoffHash {
			// Different content: whatever the last candidate did says nothing
			// about this one, so the schedule starts over.
			r.waitIntervals, r.backoffHash = 1, hash
		}
		// waitIntervals is the gap to the next attempt, so one interval means
		// the very next poll retries.
		r.ticksLeft = r.waitIntervals - 1
		r.waitIntervals = min(r.waitIntervals*2, r.maxWaitIntervals())
	} else {
		// The provider did not answer, so this poll learned nothing about the
		// candidate: retry at once. Any backoff this same content has already
		// earned stays where it is, or a single transient failure would reset
		// the schedule and the cap would bound nothing.
		r.ticksLeft = 0
	}
	r.lastOutcome, r.lastHash = ReloadRejected, hash
	return res
}

// maxWaitIntervals expresses the backoff cap in polls, so the schedule is the
// same wall-clock shape whatever the configured interval is. It is never below
// one, so a poll interval longer than the cap still retries every poll.
func (r *KeyReloader) maxWaitIntervals() int {
	return max(1, int(reloadRejectBackoffMax/r.interval))
}
