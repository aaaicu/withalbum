package password

import (
	"errors"
	"strings"
	"testing"
)

// testParams keeps tests fast. Production code always uses DefaultParams.
var testParams = Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const good = "correct horse battery"

func TestHashRoundTrip(t *testing.T) {
	h, err := Hash(good, testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("unexpected encoding prefix: %s", h)
	}
	ok, err := Verify(good, h)
	if err != nil || !ok {
		t.Fatalf("Verify(correct) = %v, %v", ok, err)
	}
	for _, wrong := range []string{"correct horse batterY", "correct horse battery ", "", strings.Repeat("x", MaxBytes+1)} {
		if ok, err := Verify(wrong, h); err != nil || ok {
			t.Fatalf("Verify(wrong %q) = %v, %v", wrong[:min(len(wrong), 30)], ok, err)
		}
	}
	h2, err := Hash(good, testParams)
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h {
		t.Fatal("two hashes of the same password are equal: salt is not random")
	}
	if !Valid(h) {
		t.Fatal("Valid(hash) = false")
	}
}

func TestDefaultParamsEncoding(t *testing.T) {
	h, err := Hash(good, DefaultParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("default parameters not recorded: %s", h)
	}
	if NeedsRehash(h, DefaultParams) {
		t.Fatal("fresh default hash needs rehash")
	}
	if ok, err := Verify(good, h); !ok || err != nil {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
}

func TestLockedNeverVerifies(t *testing.T) {
	for _, v := range []string{"!locked", "!", "!$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2U"} {
		for _, pw := range []string{"", "locked", "!locked", v, good} {
			ok, err := Verify(pw, v)
			if ok || !errors.Is(err, ErrLocked) {
				t.Fatalf("Verify(_, %q) = %v, %v; want false, ErrLocked", v, ok, err)
			}
		}
		if Valid(v) || NeedsRehash(v, DefaultParams) || !IsLocked(v) {
			t.Fatalf("%q: Valid/NeedsRehash/IsLocked wrong", v)
		}
	}
	// A locked prefix in front of a real hash must not verify either.
	h, _ := Hash(good, testParams)
	if ok, err := Verify(good, LockedPrefix+h); ok || !errors.Is(err, ErrLocked) {
		t.Fatalf("locked real hash verified: %v %v", ok, err)
	}
}

func TestTamperedRejected(t *testing.T) {
	h, err := Hash(good, testParams)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(h, "$")
	flip := func(s string) string { // change the last base64 character
		b := []byte(s)
		if b[len(b)-1] == 'A' {
			b[len(b)-1] = 'Q'
		} else {
			b[len(b)-1] = 'A'
		}
		return string(b)
	}
	mismatch := []string{
		strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], flip(parts[5])}, "$"), // key
		strings.Join([]string{"", parts[1], parts[2], parts[3], flip(parts[4]), parts[5]}, "$"), // salt
		strings.Join([]string{"", parts[1], parts[2], "m=72,t=1,p=1", parts[4], parts[5]}, "$"), // memory
		strings.Join([]string{"", parts[1], parts[2], "m=64,t=2,p=1", parts[4], parts[5]}, "$"), // time
	}
	for _, v := range mismatch {
		ok, err := Verify(good, v)
		if ok {
			t.Fatalf("tampered hash verified: %s", v)
		}
		if err != nil && !errors.Is(err, ErrMalformed) {
			t.Fatalf("unexpected error %v", err)
		}
	}
	malformed := []string{
		"",
		"plain",
		"$argon2i$v=19$m=64,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=16$m=64,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$t=1,m=64,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=064,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1,x=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=0,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=4,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=4194304,t=1,p=1$" + parts[4] + "$" + parts[5], // 4 GiB: refused before allocating
		"$argon2id$v=19$m=64,t=1000,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=300$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1$" + parts[4] + "==$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1$" + parts[4] + "$" + parts[5] + "$",
		"$argon2id$v=19$m=64,t=1,p=1$$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdA$" + parts[5],    // 4-byte salt
		"$argon2id$v=19$m=64,t=1,p=1$" + parts[4] + "$a2V5", // 3-byte key
		" " + h,
		h + "\n",
	}
	for _, v := range malformed {
		ok, err := Verify(good, v)
		if ok || !errors.Is(err, ErrMalformed) {
			t.Fatalf("Verify(%q) = %v, %v; want ErrMalformed", v, ok, err)
		}
		if Valid(v) || !NeedsRehash(v, testParams) {
			t.Fatalf("%q: Valid or NeedsRehash wrong", v)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	h, err := Hash(good, testParams)
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(h, testParams) {
		t.Fatal("same parameters need rehash")
	}
	for _, p := range []Params{
		{Memory: 128, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 64, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32},
		{Memory: 64, Time: 1, Threads: 2, SaltLen: 16, KeyLen: 32},
		{Memory: 64, Time: 1, Threads: 1, SaltLen: 32, KeyLen: 32},
		{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 64},
		DefaultParams,
	} {
		if !NeedsRehash(h, p) {
			t.Fatalf("parameters %+v: NeedsRehash = false", p)
		}
	}
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		pw   string
		want error
	}{
		{"short", ErrTooShort},
		{"elevenchars", ErrTooShort},
		{"twelve chars", nil},
		{"비밀번호는열두글자이상입니다", nil}, // counted in characters, not bytes
		{"한글열한글자입니다요", ErrTooShort},
		{strings.Repeat("a", MaxBytes), nil},
		{strings.Repeat("a", MaxBytes+1), ErrTooLong},
		{"twelve chars\n", ErrInvalidText},
		{"twelve\rchars!", ErrInvalidText},
		{"twelve\x00chars!", ErrInvalidText},
		{"twelve chars\xff", ErrInvalidText},
	}
	for _, c := range cases {
		if err := CheckPolicy(c.pw); !errors.Is(err, c.want) {
			t.Errorf("CheckPolicy(%q) = %v, want %v", c.pw[:min(len(c.pw), 20)], err, c.want)
		}
		if _, err := Hash(c.pw, testParams); !errors.Is(err, c.want) {
			t.Errorf("Hash(%q) = %v, want %v", c.pw[:min(len(c.pw), 20)], err, c.want)
		}
	}
	if _, err := Hash(good, Params{Memory: 64, Time: 0, Threads: 1, SaltLen: 16, KeyLen: 32}); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("invalid params accepted: %v", err)
	}
}

func TestErrorsDoNotLeak(t *testing.T) {
	secret := "do-not-print-this-secret"
	h, _ := Hash(secret, testParams)
	for _, err := range []error{CheckPolicy(secret + "\n"), func() error { _, e := Verify(secret, h+"x"); return e }()} {
		if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), h)) {
			t.Fatalf("error leaks secret material: %v", err)
		}
	}
}

func BenchmarkArgon2idHashDefault(b *testing.B) {
	for b.Loop() {
		if _, err := Hash(good, DefaultParams); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkArgon2idVerifyDefault(b *testing.B) {
	h, err := Hash(good, DefaultParams)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if ok, err := Verify(good, h); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}
