package account

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmailAccepts(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"plain", "a@example.test", "a@example.test"},
		{"mixed case and plus tag", "First.Last+tag@Sub.Example.TEST", "first.last+tag@sub.example.test"},
		{"surrounding spaces", "  a@example.test  ", "a@example.test"},
		{"surrounding tab and newline", "\ta@example.test\n", "a@example.test"},
		{"trailing space", "a@example.test ", "a@example.test"},
		{"already normalized", "first.last+tag@example.test", "first.last+tag@example.test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEmail(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeEmail(%q) returned error %v, want nil", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeEmailRejects(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"only spaces", "   "},
		{"no at sign", "a"},
		{"missing local part", "@example.test"},
		{"missing domain", "a@"},
		{"inner space", "a b@example.test"},
		{"display name form", "Name <a@example.test>"},
		{"double at sign", "a@@b.test"},
		{"angle brackets only", "<a@example.test>"},
		{"quoted local part", `"a"@example.test`},
		{"comma", "a,b@example.test"},
		{"over 254 bytes", strings.Repeat("a", 290) + "@example.test"},
		{"comment form", "a@example.test (comment)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEmail(tt.raw)
			if !errors.Is(err, ErrEmailInvalid) {
				t.Fatalf("NormalizeEmail(%q) error = %v, want ErrEmailInvalid", tt.raw, err)
			}
			if got != "" {
				t.Fatalf("NormalizeEmail(%q) = %q on error, want empty string", tt.raw, got)
			}
		})
	}
}

func TestNormalizeEmailIdempotent(t *testing.T) {
	accepted := []string{
		"a@example.test",
		"First.Last+tag@Sub.Example.TEST",
		"  a@example.test  ",
		"first.last+tag@example.test",
	}
	for _, raw := range accepted {
		t.Run(raw, func(t *testing.T) {
			once, err := NormalizeEmail(raw)
			if err != nil {
				t.Fatalf("first NormalizeEmail(%q) failed: %v", raw, err)
			}
			twice, err := NormalizeEmail(once)
			if err != nil {
				t.Fatalf("second NormalizeEmail(%q) failed: %v", once, err)
			}
			if once != twice {
				t.Fatalf("NormalizeEmail is not idempotent: %q then %q", once, twice)
			}
		})
	}
}

// Distinct mailboxes must stay distinct: folding Gmail dots or plus aliases
// would merge accounts that their owners consider separate.
func TestNormalizeEmailKeepsDotsAndPlusSuffix(t *testing.T) {
	got, err := NormalizeEmail("FIRST.last+tag@example.test")
	if err != nil {
		t.Fatalf("NormalizeEmail returned error %v, want nil", err)
	}
	const want = "first.last+tag@example.test"
	if got != want {
		t.Fatalf("NormalizeEmail = %q, want %q", got, want)
	}
	dotted, err := NormalizeEmail("firstlast@example.test")
	if err != nil {
		t.Fatalf("NormalizeEmail returned error %v, want nil", err)
	}
	if dotted == got {
		t.Fatalf("dot/plus folding merged %q and %q", dotted, got)
	}
}
