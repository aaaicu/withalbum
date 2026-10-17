package httpapi

import "testing"

func TestRateKey(t *testing.T) {
	for ip, want := range map[string]string{
		"192.168.1.20":              "192.168.1.20",
		"127.0.0.1":                 "127.0.0.1",
		"::1":                       "::1",
		"2001:db8:1:2:aaaa::1":      "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb:cccc::9": "2001:db8:1:2::/64",
		"fd00::1234":                "fd00::/64",
		"::ffff:192.168.1.20":       "::ffff:192.168.1.20",
		"not-an-ip":                 "not-an-ip",
	} {
		if got := rateKey(ip); got != want {
			t.Errorf("rateKey(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestHSTSHost(t *testing.T) {
	for host, want := range map[string]bool{
		"mac-mini.local":      true,
		"mac-mini.local:8787": true,
		"localhost:8787":      false,
		"LOCALHOST":           false,
		"app.localhost":       false,
		"192.168.1.20:8787":   false,
		"[fd00::1]:8787":      false,
		"":                    false,
	} {
		if got := hstsHost(host); got != want {
			t.Errorf("hstsHost(%q) = %v, want %v", host, got, want)
		}
	}
}
