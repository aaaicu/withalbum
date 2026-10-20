package relay

import (
	"context"
	"net"
	"sync"
	"time"
)

// bucket is a token bucket: rate tokens per second, at most burst stored.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

func newBucket(rate, burst float64, now time.Time) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, last: now}
}

func (b *bucket) refill(now time.Time) {
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(b.burst, b.tokens+el*b.rate)
	}
	b.last = now
}

// allow takes one token if there is one.
func (b *bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// full reports whether the bucket has refilled completely (idle key).
func (b *bucket) full(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	return b.tokens >= b.burst
}

// take removes n tokens, going into debt if needed, and returns how long
// the caller must wait for the debt to be repaid.
func (b *bucket) take(n int, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	b.tokens -= float64(n)
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens / b.rate * float64(time.Second))
}

// waitN takes n tokens and sleeps for the debt (nil bucket: no limit).
func (b *bucket) waitN(ctx context.Context, n int) error {
	if b == nil {
		return nil
	}
	d := b.take(n, time.Now())
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ipKey is the key of the per-client limits: the IP address, IPv6
// shortened to its /64 (one host may use any address of its /64).
func ipKey(addr net.Addr) string {
	var ip net.IP
	switch a := addr.(type) {
	case *net.TCPAddr:
		ip = a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return addr.String()
		}
		ip = net.ParseIP(host)
	}
	if ip == nil {
		return addr.String()
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// ipOf is the client IP text passed to the home server in OPEN.
func ipOf(addr net.Addr) string {
	if a, ok := addr.(*net.TCPAddr); ok {
		return a.IP.String()
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return host
}
