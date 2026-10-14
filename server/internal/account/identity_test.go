package account

import (
	"regexp"
	"testing"
)

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewPublicIDFormat(t *testing.T) {
	for i := 0; i < 100; i++ {
		id, err := NewPublicID()
		if err != nil {
			t.Fatalf("NewPublicID returned error %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("NewPublicID = %q (%d characters), want 36", id, len(id))
		}
		if !uuidV4Pattern.MatchString(id) {
			t.Fatalf("NewPublicID = %q, which is not a lower-case version 4 UUID", id)
		}
	}
}

func TestNewPublicIDUnique(t *testing.T) {
	const calls = 1000
	seen := make(map[string]struct{}, calls)
	for i := 0; i < calls; i++ {
		id, err := NewPublicID()
		if err != nil {
			t.Fatalf("NewPublicID returned error %v on call %d", err, i)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewPublicID returned %q twice in %d calls", id, calls)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != calls {
		t.Fatalf("got %d distinct ids, want %d", len(seen), calls)
	}
}
