package auth

import (
	"sync"

	"opentalk/server/internal/account"
)

// dummyPasswordHash keeps the unknown-address login path as expensive as a real verification. Without
// it, a fast 401 would tell an attacker which addresses have accounts.
var dummyPasswordHash = sync.OnceValue(func() string {
	digest, err := account.HashPassword("opentalk-timing-equalizer")
	if err != nil {
		// Returning an empty digest makes verification fail fast, which is only a timing leak; the
		// login still fails closed.
		return ""
	}
	return digest
})
