package account

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

var (
	ErrPasswordLength      = errors.New("password length")
	ErrPasswordComposition = errors.New("password composition")
)

const (
	// Cost parameters used for new digests. They are written into every digest,
	// so raising them later only affects newly hashed passwords.
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024 // KiB
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// Verification bounds. The cost parameters are read from stored data, and
// argon2.IDKey panics on time < 1 or threads < 1 while allocating all of its
// memory up front, so an unbounded digest would be a denial-of-service
// primitive. These caps sit far above the defaults and only reject nonsense.
const (
	maxVerifyMemory  = 1 << 21 // 2 GiB, in KiB
	maxVerifyTime    = 64
	maxVerifyThreads = 64
	maxVerifySaltLen = 64
	maxVerifyKeyLen  = 1024
)

// HashPassword returns a PHC-formatted argon2id digest with a fresh random salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	// crypto/rand.Read never reports a short read without an error, so the salt
	// is either fully random or we fail closed.
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("account: read random salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	// Unpadded standard base64 output contains no '$' or '=', which keeps the
	// digest parseable by a plain split. With the defaults below it is ~97 bytes,
	// comfortably inside the 255-byte password_hash column.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the stored PHC digest. It must never panic and
// must be safe against a malformed or truncated digest (return false).
func VerifyPassword(encodedHash, password string) bool {
	params, salt, want, ok := decodePHC(encodedHash)
	if !ok {
		return false
	}
	// The stored parameters, not the current defaults, drive the recomputation,
	// so digests written with older or stronger settings keep working. keyLen
	// comes from the stored hash so a digest of any accepted length verifies.
	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))
	// Constant time so the comparison cannot be turned into an oracle.
	return subtle.ConstantTimeCompare(got, want) == 1
}

type argon2Params struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodePHC parses `$argon2id$v=19$m=..,t=..,p=..$<b64salt>$<b64hash>`. It reports failure with a
// bool rather than an error because every parse failure means the same thing to a caller: reject the
// login. Rejecting rather than clamping malformed parameters is also what keeps a hostile digest
// from reaching argon2.IDKey.
func decodePHC(encodedHash string) (argon2Params, []byte, []byte, bool) {
	parts := strings.Split(encodedHash, "$")
	// The leading '$' yields an empty first field, so a complete digest has
	// exactly six. Missing fields, extra fields and empty input all fail here.
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argon2Params{}, nil, nil, false
	}
	// Exact match rather than an integer parse: "v=99" and any other version
	// must be rejected, and only the canonical spelling is accepted.
	if parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return argon2Params{}, nil, nil, false
	}
	// Fixed field order, as in the reference implementation and in what we emit.
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return argon2Params{}, nil, nil, false
	}
	memory, okMemory := parseUintField(fields[0], "m=", 32)
	timeCost, okTime := parseUintField(fields[1], "t=", 32)
	threads, okThreads := parseUintField(fields[2], "p=", 8)
	if !okMemory || !okTime || !okThreads {
		return argon2Params{}, nil, nil, false
	}
	if memory < 8 || memory > maxVerifyMemory ||
		timeCost < 1 || timeCost > maxVerifyTime ||
		threads < 1 || threads > maxVerifyThreads {
		return argon2Params{}, nil, nil, false
	}
	// Raw (unpadded) encoding only, matching what HashPassword emits. Decoding is
	// strict, so stray padding or non-alphabet bytes are rejected here.
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > maxVerifySaltLen {
		return argon2Params{}, nil, nil, false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 || len(key) > maxVerifyKeyLen {
		return argon2Params{}, nil, nil, false
	}
	return argon2Params{memory: uint32(memory), time: uint32(timeCost), threads: uint8(threads)}, salt, key, true
}

// parseUintField reads `<prefix><unsigned decimal>` with no sign, space or trailing bytes.
func parseUintField(field, prefix string, bitSize int) (uint64, bool) {
	if !strings.HasPrefix(field, prefix) {
		return 0, false
	}
	v, err := strconv.ParseUint(field[len(prefix):], 10, bitSize)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ValidatePassword reports whether password satisfies the composition policy.
// Length is checked first, so a too-short password is reported as a length
// problem even when it also lacks a digit.
func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < PasswordMinRunes || n > PasswordMaxRunes {
		return ErrPasswordLength
	}
	var hasLetter, hasDigit bool
	for _, r := range password {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	// Only a class requirement, never a dictionary or breach check: those belong
	// in front of this call, where they can be rate limited.
	if !hasLetter || !hasDigit {
		return ErrPasswordComposition
	}
	return nil
}
