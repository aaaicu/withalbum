package main

import (
	"fmt"
	"testing"
)

func TestClientIDsSetting(t *testing.T) {
	t.Setenv("WITHALBUM_TEST_IDS", "")
	for _, c := range []struct {
		flag, env, def string
		want           string
	}{
		{"", "", "aaaicu.withalbum.app", "[aaaicu.withalbum.app]"},
		{"", "a, b ,", "def", "[a b]"},
		{"x", "a", "def", "[x]"},
		{"off", "a", "def", "[]"},
		{"", "OFF", "def", "[]"},
		{"", "", "", "[]"},
	} {
		t.Setenv("WITHALBUM_TEST_IDS", c.env)
		if got := fmt.Sprint(clientIDsSetting(c.flag, "WITHALBUM_TEST_IDS", c.def)); got != c.want {
			t.Errorf("clientIDsSetting(%q, env %q, %q) = %s, want %s", c.flag, c.env, c.def, got, c.want)
		}
	}
	if fmt.Sprint(sortedProviders([]string{"apple", "google"})) != "[google apple]" {
		t.Fatal("order")
	}
}
