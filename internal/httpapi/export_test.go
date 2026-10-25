package httpapi

import "time"

// SetSocialClock replaces the clock of the social write limiter (tests
// only; call before the first request).
func SetSocialClock(s *Server, now func() time.Time) {
	s.socialLimit.mu.Lock()
	defer s.socialLimit.mu.Unlock()
	s.socialLimit.now = now
}
