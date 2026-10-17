package password

import (
	"context"
	"errors"
	"time"
)

// ErrBusy: every hashing slot stayed taken for the limiter's wait time (or
// until the context ended). The
// caller should answer "try again shortly" (HTTP 503 with Retry-After).
var ErrBusy = errors.New("password hashing is busy")

// Limiter bounds how many Argon2id computations run at the same time. Each
// one allocates Params.Memory (64 MiB by default), so an unbounded number of
// concurrent logins could exhaust the memory of a small server (Raspberry
// Pi). Every network-facing hash or verification must go through a Limiter.
//
// It also verifies against a dummy hash when there is no usable stored hash
// (unknown account, locked bootstrap account, malformed value), so that a
// login attempt costs the same time whether or not the account exists.
type Limiter struct {
	slots  chan struct{}
	wait   time.Duration
	params Params
	dummy  string
}

// NewLimiter returns a Limiter running at most n computations at once
// (n < 1 means 1), letting a caller wait at most wait for a slot (<= 0: 5 s),
// and hashing new passwords with p. It computes the dummy hash once, with p,
// so it takes as long as one hash.
func NewLimiter(n int, wait time.Duration, p Params) (*Limiter, error) {
	if n < 1 {
		n = 1
	}
	if wait <= 0 {
		wait = 5 * time.Second
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	dummy, err := Hash("withalbum dummy password for timing", p)
	if err != nil {
		return nil, err
	}
	return &Limiter{slots: make(chan struct{}, n), wait: wait, params: p, dummy: dummy}, nil
}

// Params returns the parameters new hashes are made with.
func (l *Limiter) Params() Params { return l.params }

func (l *Limiter) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	default:
	}
	t := time.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-t.C:
		return ErrBusy
	case <-ctx.Done():
		return ErrBusy
	}
}

func (l *Limiter) release() { <-l.slots }

// Hash is Hash(pw, l.Params()) inside a slot. Policy errors return before
// waiting for a slot.
func (l *Limiter) Hash(ctx context.Context, pw string) (string, error) {
	if err := CheckPolicy(pw); err != nil {
		return "", err
	}
	if err := l.acquire(ctx); err != nil {
		return "", err
	}
	defer l.release()
	return Hash(pw, l.params)
}

// Rehash is Hash(pw, l.Params()) inside a slot without the policy check: it
// re-hashes a password that was just verified (stored with older
// parameters), which may predate the current policy. Passwords longer than
// MaxBytes are refused.
func (l *Limiter) Rehash(ctx context.Context, pw string) (string, error) {
	if len(pw) > MaxBytes {
		return "", errors.New("password: too long")
	}
	if err := l.acquire(ctx); err != nil {
		return "", err
	}
	defer l.release()
	return Hash(pw, l.params)
}

// Verify reports whether pw matches encoded, inside a slot. When encoded is
// not a usable hash (empty because the account does not exist, locked or
// malformed) it verifies pw against the dummy hash instead and returns
// false, so the time spent does not reveal which case applied. The only
// error is ErrBusy.
func (l *Limiter) Verify(ctx context.Context, pw, encoded string) (bool, error) {
	if err := l.acquire(ctx); err != nil {
		return false, err
	}
	defer l.release()
	usable := Valid(encoded)
	if !usable {
		encoded = l.dummy
	}
	ok, err := Verify(pw, encoded)
	return ok && usable && err == nil, nil
}
