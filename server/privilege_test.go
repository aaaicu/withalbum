package main

import "testing"

func TestRefuseRoot(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if refuseRoot(501, get) != nil || refuseRoot(-1, get) != nil {
		t.Fatal("an ordinary user was refused")
	}
	if refuseRoot(0, get) == nil {
		t.Fatal("root was not refused")
	}
	env["WITHALBUM_ALLOW_ROOT"] = "yes"
	if refuseRoot(0, get) == nil {
		t.Fatal("only the exact value 1 overrides")
	}
	env["WITHALBUM_ALLOW_ROOT"] = "1"
	if refuseRoot(0, get) != nil {
		t.Fatal("override ignored")
	}
}
