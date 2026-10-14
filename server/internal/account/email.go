package account

import (
	"errors"
	"net/mail"
	"strings"
	"unicode"
)

// ErrEmailInvalid reports any address we refuse to store. It is deliberately
// coarse: telling a caller *why* an address was rejected leaks nothing useful to
// them and only helps an attacker enumerate which forms the validator accepts.
var ErrEmailInvalid = errors.New("email invalid")

// maxEmailBytes is the RFC 5321 limit on the total length of a reverse-path.
// It is a byte limit, unlike the rune limits in validate.go, because the SMTP
// limit applies to the on-the-wire octets.
const maxEmailBytes = 254

// NormalizeEmail trims surrounding space, validates the address, and applies Unicode lower-case
// folding. It deliberately does NOT fold Gmail dots or plus aliases: treating distinct addresses
// as one account would be a security and support problem.
func NormalizeEmail(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxEmailBytes {
		return "", ErrEmailInvalid
	}
	// net/mail.ParseAddress happily unwraps display-name and comment forms, so
	// reject their delimiters up front. Silently accepting `Name <a@b.test>`
	// would let a caller store something that is not an address at all.
	if strings.ContainsAny(trimmed, `<>",`) {
		return "", ErrEmailInvalid
	}
	// Any whitespace left after trimming is inner whitespace. Some forms such as
	// "a b@example.test" survive ParseAddress, so reject them here.
	for _, r := range trimmed {
		if unicode.IsSpace(r) {
			return "", ErrEmailInvalid
		}
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", ErrEmailInvalid
	}
	// ParseAddress is lenient (it strips comments and folds display names), so
	// require the parse to be the identity. This is what makes the check above
	// structural rather than a blocklist of characters we happened to think of.
	if addr.Address != trimmed {
		return "", ErrEmailInvalid
	}
	// Exactly one '@' with non-empty sides. ParseAddress already enforces this
	// for the forms it accepts, but the invariant is cheap to state and keeps
	// NormalizeEmail correct even if net/mail gets more permissive later.
	if strings.Count(trimmed, "@") != 1 {
		return "", ErrEmailInvalid
	}
	local, domain, _ := strings.Cut(trimmed, "@")
	if local == "" || domain == "" {
		return "", ErrEmailInvalid
	}
	// Case folding only. Local parts are technically case sensitive, but every
	// provider we care about is case insensitive and folding keeps lookups by
	// normalized address working. Trimming occurrences of '.' or '+' is not done
	// on purpose: merging distinct mailboxes would merge distinct accounts.
	return strings.ToLower(trimmed), nil
}
