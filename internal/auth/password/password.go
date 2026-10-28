// Package password hashes and verifies account passwords with Argon2id.
//
// A hash is stored as a PHC string that records the algorithm, the Argon2
// version and every cost parameter next to the salt and the derived key:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt, base64>$<key, base64>
//
// so DefaultParams can change later without invalidating stored hashes;
// NeedsRehash tells when a stored hash uses other parameters than the current
// ones (rehash it after the next successful login).
//
// Values starting with LockedPrefix ("!") mark accounts that cannot log in
// (for example the locked bootstrap administrator). Verify never accepts them,
// whatever password is given. Any value that is not a canonical PHC string
// produced by this package is rejected the same way.
//
// Errors and String methods never contain a password or a hash.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Policy for new passwords.
const (
	// MinRunes is the minimum length of a new password in characters.
	MinRunes = 12
	// MaxBytes bounds a password in bytes (UTF-8). Longer input is never
	// hashed, so it cannot be used to make the server do unbounded work.
	MaxBytes = 1024
)

// LockedPrefix starts every stored value that must never verify.
const LockedPrefix = "!"

// Params are the Argon2id cost parameters.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // passes
	Threads uint8  // lanes
	SaltLen uint32 // bytes
	KeyLen  uint32 // bytes
}

// DefaultParams is used for every new hash: 64 MiB, 3 passes, 2 lanes,
// 16-byte random salt, 32-byte key (OWASP recommended range). Each hash
// computation allocates Memory, so concurrent logins must be bounded by the
// caller.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// Bounds accepted when hashing and when parsing a stored hash. They stop a
// tampered hash from making a verification allocate gigabytes or run for
// minutes.
const (
	minMemory  = 8       // KiB; Argon2 also needs at least 8*Threads
	maxMemory  = 1 << 20 // KiB (1 GiB)
	maxTime    = 16
	maxThreads = 16
	minSaltLen = 8
	maxSaltLen = 64
	minKeyLen  = 16
	maxKeyLen  = 64
)

var (
	// ErrTooShort: a new password has fewer than MinRunes characters.
	ErrTooShort = fmt.Errorf("password must be at least %d characters", MinRunes)
	// ErrTooLong: a password is longer than MaxBytes bytes.
	ErrTooLong = fmt.Errorf("password must be at most %d bytes", MaxBytes)
	// ErrInvalidText: a password is not valid UTF-8 or contains a NUL or a
	// line break (they cannot be typed reliably and break stdin input).
	ErrInvalidText = errors.New("password must be valid UTF-8 text without NUL or line breaks")
	// ErrLocked: the stored value is a locked marker; it never verifies.
	ErrLocked = errors.New("account password is locked")
	// ErrMalformed: the stored value is not a canonical Argon2id PHC string
	// within the accepted parameter bounds.
	ErrMalformed = errors.New("stored password hash is malformed or unsupported")
	// ErrInvalidParams: Params outside the accepted bounds.
	ErrInvalidParams = errors.New("invalid argon2id parameters")
)

// CheckPolicy reports whether pw is acceptable as a new password.
func CheckPolicy(pw string) error {
	if len(pw) > MaxBytes {
		return ErrTooLong
	}
	if !utf8.ValidString(pw) || strings.ContainsAny(pw, "\x00\r\n") {
		return ErrInvalidText
	}
	if utf8.RuneCountInString(pw) < MinRunes {
		return ErrTooShort
	}
	return nil
}

func (p Params) validate() error {
	if p.Memory < minMemory || p.Memory > maxMemory || p.Memory < 8*uint32(p.Threads) ||
		p.Time < 1 || p.Time > maxTime ||
		p.Threads < 1 || p.Threads > maxThreads ||
		p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen ||
		p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen {
		return ErrInvalidParams
	}
	return nil
}

// Hash checks pw against CheckPolicy and returns its Argon2id PHC string
// with a new random salt.
func Hash(pw string, p Params) (string, error) {
	if err := CheckPolicy(pw); err != nil {
		return "", err
	}
	if err := p.validate(); err != nil {
		return "", err
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: random salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return encode(p, salt, key), nil
}

var b64 = base64.RawStdEncoding.Strict()

func encode(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// decoded is a parsed stored hash.
type decoded struct {
	params    Params
	salt, key []byte
}

// parse accepts only the exact canonical form encode produces, with
// parameters inside the bounds.
func parse(encoded string) (decoded, error) {
	if IsLocked(encoded) {
		return decoded{}, ErrLocked
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return decoded{}, ErrMalformed
	}
	var d decoded
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return decoded{}, ErrMalformed
	}
	vals := make([]uint64, 3)
	for i, name := range []string{"m=", "t=", "p="} {
		v, ok := strings.CutPrefix(kv[i], name)
		if !ok {
			return decoded{}, ErrMalformed
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return decoded{}, ErrMalformed
		}
		vals[i] = n
	}
	if vals[2] > maxThreads {
		return decoded{}, ErrMalformed
	}
	var err error
	if d.salt, err = b64.DecodeString(parts[4]); err != nil {
		return decoded{}, ErrMalformed
	}
	if d.key, err = b64.DecodeString(parts[5]); err != nil {
		return decoded{}, ErrMalformed
	}
	d.params = Params{Memory: uint32(vals[0]), Time: uint32(vals[1]), Threads: uint8(vals[2]),
		SaltLen: uint32(len(d.salt)), KeyLen: uint32(len(d.key))}
	if d.params.validate() != nil || encode(d.params, d.salt, d.key) != encoded {
		return decoded{}, ErrMalformed
	}
	return d, nil
}

// IsLocked reports whether a stored value is a locked marker.
func IsLocked(encoded string) bool { return strings.HasPrefix(encoded, LockedPrefix) }

// Valid reports whether encoded is a usable stored hash (canonical, within
// bounds, not locked).
func Valid(encoded string) bool {
	_, err := parse(encoded)
	return err == nil
}

// Verify reports whether pw matches the stored hash, comparing the derived
// keys in constant time. A locked or malformed stored value returns false
// with ErrLocked or ErrMalformed. A password longer than MaxBytes returns
// false without hashing. Verify does not apply the length minimum, so
// passwords set under an older policy still work.
func Verify(pw, encoded string) (bool, error) {
	d, err := parse(encoded)
	if err != nil {
		return false, err
	}
	if len(pw) > MaxBytes {
		return false, nil
	}
	key := argon2.IDKey([]byte(pw), d.salt, d.params.Time, d.params.Memory, d.params.Threads, d.params.KeyLen)
	return subtle.ConstantTimeCompare(key, d.key) == 1, nil
}

// NeedsRehash reports whether a stored hash should be replaced by one made
// with p: its parameters differ or it cannot be parsed. Locked values are
// never "upgraded" (false).
func NeedsRehash(encoded string, p Params) bool {
	if IsLocked(encoded) {
		return false
	}
	d, err := parse(encoded)
	if err != nil {
		return true
	}
	return d.params != p
}
