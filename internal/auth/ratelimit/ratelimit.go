// Package ratelimit throttles login attempts in memory.
//
// Three independent limits apply to every attempt:
//
//   - per client IP: a token bucket over all attempts (successful or not),
//     which also bounds how often one client makes the server hash;
//   - per account and IP: consecutive failures within a window;
//   - per account from any IP: a higher failure count within the window, so
//     guessing one account from many addresses is slowed down as well. It
//     is counted per Scope: failures that arrive from outside the home
//     (relay, Tailscale Funnel: ScopeRemote) never block the same account
//     on this Mac or the home network (ScopeLocal), so an internet attacker
//     who knows a login name cannot lock its owner out of logging in (or
//     pairing a new phone) at home.
//
// The account key is the login name as typed (case-folded), whether or not
// such an account exists, so a throttled answer reveals nothing about which
// accounts exist. A success clears the failures of that account and IP pair
// (not the global account counter, so an attacker who also knows a valid
// password elsewhere cannot reset it).
//
// State is in memory only: a restart forgets it. The number of tracked keys
// is bounded; when the table is full of live entries, new keys are refused
// (fail closed) until entries expire.
package ratelimit

import (
	"strings"
	"sync"
	"time"
)

// Config sets the limits. Zero fields take the defaults of DefaultConfig.
type Config struct {
	// IPBurst attempts may be made at once from one IP; afterwards one more
	// every IPInterval.
	IPBurst    int
	IPInterval time.Duration
	// After PairFailures failed attempts on one account from one IP within
	// Window, further attempts on it from that IP are refused until the
	// window of the first failure ends.
	PairFailures int
	// The same for AccountFailures failures on one account from any IP.
	AccountFailures int
	Window          time.Duration
	// MaxKeys bounds each table.
	MaxKeys int
	// Now is the clock (tests); nil means time.Now.
	Now func() time.Time
}

// DefaultConfig: 10 attempts at once per IP then 1 every 6 s (10/min);
// 5 failures per account and IP and 20 per account in 15 minutes.
var DefaultConfig = Config{IPBurst: 10, IPInterval: 6 * time.Second, PairFailures: 5,
	AccountFailures: 20, Window: 15 * time.Minute, MaxKeys: 10000}

type bucket struct {
	tokens float64
	last   time.Time
}

type failures struct {
	count int
	first time.Time
}

// Limiter is safe for concurrent use.
type Limiter struct {
	cfg     Config
	mu      sync.Mutex
	ips     map[string]*bucket
	pairs   map[string]*failures
	account map[string]*failures
}

// New returns a Limiter for cfg.
func New(cfg Config) *Limiter {
	d := DefaultConfig
	if cfg.IPBurst <= 0 {
		cfg.IPBurst = d.IPBurst
	}
	if cfg.IPInterval <= 0 {
		cfg.IPInterval = d.IPInterval
	}
	if cfg.PairFailures <= 0 {
		cfg.PairFailures = d.PairFailures
	}
	if cfg.AccountFailures <= 0 {
		cfg.AccountFailures = d.AccountFailures
	}
	if cfg.Window <= 0 {
		cfg.Window = d.Window
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = d.MaxKeys
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Limiter{cfg: cfg, ips: map[string]*bucket{}, pairs: map[string]*failures{}, account: map[string]*failures{}}
}

// accountKey case-folds and bounds the typed login name.
func accountKey(login string) string {
	if len(login) > 64 {
		login = login[:64]
	}
	return strings.ToLower(login)
}

func pairKey(ip, login string) string { return ip + "\x00" + accountKey(login) }

// Scope is where a login attempt comes from, for the account-wide limit.
type Scope string

const (
	// ScopeLocal: this Mac or the home network (the main listener).
	ScopeLocal Scope = ""
	// ScopeRemote: from outside the home (relay, Tailscale Funnel).
	ScopeRemote Scope = "remote"
)

func scopedAccountKey(sc Scope, login string) string { return string(sc) + "\x00" + accountKey(login) }

// blockedFor returns how long f still blocks at limit, or 0.
func (l *Limiter) blockedFor(f *failures, limit int, now time.Time) time.Duration {
	if f == nil || f.count < limit {
		return 0
	}
	if end := f.first.Add(l.cfg.Window); now.Before(end) {
		return end.Sub(now)
	}
	return 0
}

// Allow is AllowIn(ScopeLocal, ip, login).
func (l *Limiter) Allow(ip, login string) (ok bool, retryAfter time.Duration) {
	return l.AllowIn(ScopeLocal, ip, login)
}

// AllowIn records an attempt for login from ip in scope sc and reports
// whether it may proceed. When it may not, retryAfter is how long to wait
// (at least 1 s).
func (l *Limiter) AllowIn(sc Scope, ip, login string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	wait := max(l.blockedFor(l.pairs[pairKey(ip, login)], l.cfg.PairFailures, now),
		l.blockedFor(l.account[scopedAccountKey(sc, login)], l.cfg.AccountFailures, now))
	if wait > 0 {
		return false, max(wait, time.Second)
	}
	b := l.ips[ip]
	if b == nil {
		if len(l.ips) >= l.cfg.MaxKeys {
			l.pruneLocked(now)
			if len(l.ips) >= l.cfg.MaxKeys {
				return false, time.Minute
			}
		}
		b = &bucket{tokens: float64(l.cfg.IPBurst), last: now}
		l.ips[ip] = b
	}
	b.tokens = min(float64(l.cfg.IPBurst), b.tokens+float64(now.Sub(b.last))/float64(l.cfg.IPInterval))
	b.last = now
	if b.tokens < 1 {
		return false, max(time.Duration((1-b.tokens)*float64(l.cfg.IPInterval)), time.Second)
	}
	b.tokens--
	return true, 0
}

// addFailure counts a failure in m under key and reports whether the count
// just reached limit (the key is blocked from now on).
func (l *Limiter) addFailure(m map[string]*failures, key string, limit int, now time.Time) bool {
	f := m[key]
	if f != nil && !now.Before(f.first.Add(l.cfg.Window)) {
		f = nil // window over: start again
	}
	if f == nil {
		if len(m) >= l.cfg.MaxKeys {
			l.pruneLocked(now)
		}
		if len(m) >= l.cfg.MaxKeys {
			return false
		}
		f = &failures{first: now}
		m[key] = f
	}
	f.count++
	return f.count == limit
}

// Failure is FailureIn(ScopeLocal, ip, login).
func (l *Limiter) Failure(ip, login string) (nowBlocked bool) {
	return l.FailureIn(ScopeLocal, ip, login)
}

// FailureIn records a failed attempt in scope sc. It reports whether this
// failure started a block (for an audit entry written once per block, not
// per attempt).
func (l *Limiter) FailureIn(sc Scope, ip, login string) (nowBlocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.cfg.Now()
	a := l.addFailure(l.pairs, pairKey(ip, login), l.cfg.PairFailures, now)
	b := l.addFailure(l.account, scopedAccountKey(sc, login), l.cfg.AccountFailures, now)
	return a || b
}

// Success clears the failures of login from ip.
func (l *Limiter) Success(ip, login string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pairs, pairKey(ip, login))
}

// pruneLocked drops entries that no longer affect any decision.
func (l *Limiter) pruneLocked(now time.Time) {
	for k, b := range l.ips {
		if now.Sub(b.last) >= time.Duration(l.cfg.IPBurst)*l.cfg.IPInterval {
			delete(l.ips, k)
		}
	}
	for _, m := range []map[string]*failures{l.pairs, l.account} {
		for k, f := range m {
			if !now.Before(f.first.Add(l.cfg.Window)) {
				delete(m, k)
			}
		}
	}
}
