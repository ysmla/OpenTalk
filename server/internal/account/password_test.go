package account

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"minimum length with letter and digit", "abcd1234", nil},
		{"maximum length with letter and digit", "abcdefgh12345678", nil},
		{"two chinese letters and eight digits", "密码12345678", nil},
		{"digits and letters mixed", "a1b2c3d4", nil},
		{"one below minimum", "abc1234", ErrPasswordLength},
		{"one above maximum", "abcdefgh123456789", ErrPasswordLength},
		{"empty", "", ErrPasswordLength},
		{"digits only", "12345678", ErrPasswordComposition},
		{"letters only", "abcdefgh", ErrPasswordComposition},
		{"symbols only", "!!!!!!!!", ErrPasswordComposition},
		{"too short and digits only reports length", "1234567", ErrPasswordLength},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidatePassword(%q) = %v, want %v", tt.password, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePasswordCountsRunesNotBytes(t *testing.T) {
	// Eight Chinese letters plus one digit is nine runes but 25 bytes, so a byte
	// based limit would reject it and a rune based one accepts it.
	password := "密码密码密码密码1"
	if len(password) <= PasswordMaxRunes {
		t.Fatalf("fixture is not byte-long enough to be meaningful: %d bytes", len(password))
	}
	if err := ValidatePassword(password); err != nil {
		t.Fatalf("ValidatePassword(%q) = %v, want nil", password, err)
	}
	// Ten Chinese runes are ten runes but thirty bytes, so a byte based limit
	// would reject what the rune budget accepts.
	if err := ValidatePassword("密码密码密码密码密1"); err != nil {
		t.Fatalf("ValidatePassword of ten runes = %v, want nil", err)
	}
}

func TestHashPasswordVerifyPasswordRoundTrip(t *testing.T) {
	passwords := []string{
		"abcd1234",
		"abcdefgh12345678",
		"密码12345678",
		"p@ss word!\"'\\",
		strings.Repeat("a1", 8),
	}
	for _, password := range passwords {
		t.Run(password, func(t *testing.T) {
			hash, err := HashPassword(password)
			if err != nil {
				t.Fatalf("HashPassword(%q) returned error %v", password, err)
			}
			if !VerifyPassword(hash, password) {
				t.Fatalf("VerifyPassword rejected the password it was built from")
			}
			if VerifyPassword(hash, password+"x") {
				t.Fatalf("VerifyPassword accepted a wrong password")
			}
			if VerifyPassword(hash, "") {
				t.Fatalf("VerifyPassword accepted an empty password")
			}
		})
	}
}

func TestHashPasswordUsesFreshSalt(t *testing.T) {
	const password = "abcd1234"
	first, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword returned error %v", err)
	}
	second, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword returned error %v", err)
	}
	if first == second {
		t.Fatalf("two hashes of the same password are identical, so the salt is not random")
	}
	// Both must still verify: a differing salt must not change the outcome.
	if !VerifyPassword(first, password) || !VerifyPassword(second, password) {
		t.Fatalf("at least one of two digests failed to verify")
	}
}

func TestHashPasswordFormatAndLength(t *testing.T) {
	hash, err := HashPassword("abcd1234")
	if err != nil {
		t.Fatalf("HashPassword returned error %v", err)
	}
	const prefix = "$argon2id$v=19$m=65536,t=3,p=2$"
	if !strings.HasPrefix(hash, prefix) {
		t.Fatalf("HashPassword = %q, want prefix %q", hash, prefix)
	}
	t.Logf("digest is %d bytes: %s", len(hash), hash)
	// The password_hash column is VARCHAR(255); a longer digest would be
	// truncated by MySQL and become unverifiable.
	if len(hash) >= 255 {
		t.Fatalf("digest is %d bytes, must stay under the 255 byte column", len(hash))
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Fatalf("digest %q has %d PHC fields, want 6", hash, len(parts))
	}
	if strings.Contains(parts[4], "=") || strings.Contains(parts[5], "=") {
		t.Fatalf("digest %q uses padded base64, want unpadded", hash)
	}
}

// Parameters travel inside the digest, so a digest written with weaker settings
// than the current defaults must still verify, using its own settings.
func TestVerifyPasswordHonoursStoredParameters(t *testing.T) {
	const password = "abcd1234"
	salt := []byte("0123456789abcdef")
	// Deliberately not the current defaults: m=8192,t=1,p=1.
	key := argon2.IDKey([]byte(password), salt, 1, 8192, 1, argonKeyLen)
	digest := fmt.Sprintf("$argon2id$v=%d$m=8192,t=1,p=1$%s$%s",
		argon2.Version,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))

	params, gotSalt, gotKey, ok := decodePHC(digest)
	if !ok {
		t.Fatalf("decodePHC rejected a digest we just built: %q", digest)
	}
	if params.memory != 8192 || params.time != 1 || params.threads != 1 {
		t.Fatalf("decodePHC read m=%d,t=%d,p=%d, want m=8192,t=1,p=1",
			params.memory, params.time, params.threads)
	}
	if !bytes.Equal(gotSalt, salt) || !bytes.Equal(gotKey, key) {
		t.Fatalf("decodePHC did not round-trip the salt and key")
	}
	if !VerifyPassword(digest, password) {
		t.Fatalf("VerifyPassword rejected a valid digest carrying non-default parameters")
	}
	if VerifyPassword(digest, "wrong1234") {
		t.Fatalf("VerifyPassword accepted a wrong password for a non-default digest")
	}
	// Control: the same password and salt hashed with the *default* parameters
	// gives a different key, so the assertions above can only hold if the stored
	// parameters rather than the defaults were used.
	defaultKey := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	if bytes.Equal(defaultKey, key) {
		t.Fatalf("default and non-default parameters produced the same key, so the fixture proves nothing")
	}
}

func TestVerifyPasswordRejectsTamperedDigest(t *testing.T) {
	hash, err := HashPassword("abcd1234")
	if err != nil {
		t.Fatalf("HashPassword returned error %v", err)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected digest shape %q", hash)
	}
	// Flip one character of the encoded hash to a different base64 character.
	tampered := []byte(parts[5])
	if tampered[0] == 'A' {
		tampered[0] = 'B'
	} else {
		tampered[0] = 'A'
	}
	broken := strings.Join([]string{parts[0], parts[1], parts[2], parts[3], parts[4], string(tampered)}, "$")
	if VerifyPassword(broken, "abcd1234") {
		t.Fatalf("VerifyPassword accepted a tampered digest")
	}
	// Tampering the salt must also fail.
	brokenSalt := strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "AAAAAAAAAAAAAAAAAAAAAA", parts[5]}, "$")
	if VerifyPassword(brokenSalt, "abcd1234") {
		t.Fatalf("VerifyPassword accepted a tampered salt")
	}
}

func TestVerifyPasswordMalformedDigests(t *testing.T) {
	valid, err := HashPassword("abcd1234")
	if err != nil {
		t.Fatalf("HashPassword returned error %v", err)
	}
	validParts := strings.Split(valid, "$")
	tests := []struct {
		name  string
		hash  string
		notes string
	}{
		{"empty", "", "no fields at all"},
		{"no dollar signs", "not-a-hash", ""},
		{"wrong prefix", valid[len("$argon2id"):], "missing leading algorithm field"},
		{"wrong algorithm", strings.Replace(valid, "$argon2id$", "$argon2i$", 1), "argon2i is not accepted"},
		{"bcrypt digest", "$2y$10$abcdefghijklmnopqrstuv", "another scheme entirely"},
		{"missing salt and hash", "$argon2id$v=19$m=65536,t=3,p=2", "fewer than six fields"},
		{"missing hash", strings.Join(validParts[:5], "$"), "truncated after salt"},
		{"truncated mid hash", valid[:len(valid)-10], "cut inside the encoded hash"},
		{"non base64 salt", strings.Join([]string{"", "argon2id", "v=19", "m=65536,t=3,p=2", "!!!!", validParts[5]}, "$"), ""},
		{"non base64 hash", strings.Join([]string{"", "argon2id", "v=19", "m=65536,t=3,p=2", validParts[4], "!!!!"}, "$"), ""},
		{"padded base64", strings.Join([]string{"", "argon2id", "v=19", "m=65536,t=3,p=2", validParts[4] + "==", validParts[5]}, "$"), ""},
		{"version 99", strings.Replace(valid, "v=19", "v=99", 1), "unknown version"},
		{"version missing", strings.Replace(valid, "v=19", "v=", 1), ""},
		{"memory zero", strings.Replace(valid, "m=65536", "m=0", 1), "threads/parallelism of zero panics argon2"},
		{"time zero", strings.Replace(valid, "t=3", "t=0", 1), ""},
		{"threads zero", strings.Replace(valid, "p=2", "p=0", 1), "zero parallelism panics argon2.IDKey"},
		{"memory absurd", strings.Replace(valid, "m=65536", "m=4294967295", 1), "unbounded allocation is a DoS primitive"},
		{"negative cost", strings.Replace(valid, "t=3", "t=-3", 1), ""},
		{"non numeric cost", strings.Replace(valid, "m=65536", "m=abc", 1), ""},
		{"cost overflow", strings.Replace(valid, "m=65536", "m=99999999999999999999", 1), ""},
		{"reordered fields", strings.Replace(valid, "m=65536,t=3,p=2", "t=3,m=65536,p=2", 1), ""},
		{"extra cost field", strings.Replace(valid, "m=65536,t=3,p=2", "m=65536,t=3,p=2,x=1", 1), ""},
		{"extra trailing field", valid + "$", ""},
		{"salt too short", strings.Join([]string{"", "argon2id", "v=19", "m=65536,t=3,p=2", "AAAA", validParts[5]}, "$"), ""},
		{"empty hash field", strings.Join([]string{"", "argon2id", "v=19", "m=65536,t=3,p=2", validParts[4], ""}, "$"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The contract is "never panic, always false"; a panic here fails the
			// subtest rather than the whole process.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("VerifyPassword panicked on %q: %v", tt.hash, r)
				}
			}()
			if VerifyPassword(tt.hash, "abcd1234") {
				t.Fatalf("VerifyPassword accepted malformed digest %q (%s)", tt.hash, tt.notes)
			}
		})
	}
}
