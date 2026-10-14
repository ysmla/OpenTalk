package account

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewPublicID returns a random RFC 4122 version 4 UUID string (36 characters, lower-case hex).
func NewPublicID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("account: read random uuid: %w", err)
	}
	// Stamp version 4 into the high nibble of byte 6 and the RFC 4122 variant
	// (10xx) into the high bits of byte 8. Without this the bytes are random but
	// the string is not a valid v4 UUID and would fail clients that parse it.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	// Formatted by hand rather than with a UUID dependency: this is the only
	// place the project needs one, and hex.Encode emits lower-case already.
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:]), nil
}
