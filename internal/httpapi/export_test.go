package httpapi

import "time"

// SetSocialClock replaces the clock of the social write limiter (tests
// only; call before the first request).
func SetSocialClock(s *Server, now func() time.Time) {
	s.socialLimit.mu.Lock()
	defer s.socialLimit.mu.Unlock()
	s.socialLimit.now = now
}

// SetPlaybackClock replaces the playback worker's clock (retry times).
func SetPlaybackClock(s *Server, now func() time.Time) { s.playbackNow = now }

// SetPlaybackLimit replaces the time a playback tool gets.
func SetPlaybackLimit(s *Server, limit func(durMS int64) time.Duration) { s.playbackLimit = limit }

// SetPlaybackGap replaces the pause between two conversions.
func SetPlaybackGap(s *Server, d time.Duration) { s.playbackGap = d }
