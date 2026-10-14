package secretbox

import (
	"bytes"
	"errors"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := Derive([]byte("0123456789abcdef0123456789abcdef"), "test-purpose")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestDeriveBindsKeysToPurpose(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	first, err := Derive(secret, "one")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != KeyBytes {
		t.Fatalf("key length=%d want=%d", len(first), KeyBytes)
	}
	// The same label must be reproducible, or a restart would make stored ciphertext unreadable.
	again, err := Derive(secret, "one")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, again) {
		t.Fatal("derivation is not deterministic")
	}
	// A different label must not produce the same key, otherwise one leaked subkey opens everything.
	other, err := Derive(secret, "two")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, other) {
		t.Fatal("different labels produced the same key")
	}
	if _, err := Derive([]byte("too-short"), "one"); err == nil {
		t.Fatal("a short secret was accepted")
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key := testKey(t)
	for name, plaintext := range map[string][]byte{
		"empty":   {},
		"ascii":   []byte("smtp-password"),
		"unicode": []byte("邮件密码"),
		"long":    bytes.Repeat([]byte("x"), 4096),
		"binary":  {0x00, 0xff, 0x10, 0x00},
	} {
		t.Run(name, func(t *testing.T) {
			sealed, err := Seal(key, plaintext)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(sealed, plaintext) && len(plaintext) > 0 {
				t.Fatal("ciphertext contains the plaintext")
			}
			opened, err := Open(key, sealed)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(opened, plaintext) {
				t.Fatalf("round trip changed the value: %q", opened)
			}
		})
	}
}

func TestSealUsesFreshNonce(t *testing.T) {
	key := testKey(t)
	first, err := Seal(key, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Seal(key, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	// Identical ciphertext would let an observer tell that two stored values are the same secret.
	if bytes.Equal(first, second) {
		t.Fatal("sealing the same plaintext twice produced identical ciphertext")
	}
}

func TestOpenRejectsWrongKeyTamperedAndMalformed(t *testing.T) {
	key := testKey(t)
	sealed, err := Seal(key, []byte("smtp-password"))
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := Derive([]byte("0123456789abcdef0123456789abcdef"), "other-purpose")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(otherKey, sealed); err == nil {
		t.Fatal("a different key decrypted the value")
	}

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := Open(key, tampered); err == nil {
		t.Fatal("a tampered ciphertext was accepted")
	}

	for name, value := range map[string][]byte{
		"nil":        nil,
		"empty":      {},
		"nonce only": bytes.Repeat([]byte{0x01}, 12),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Open(key, value); !errors.Is(err, ErrMalformed) {
				t.Fatalf("error=%v want ErrMalformed", err)
			}
		})
	}
}
