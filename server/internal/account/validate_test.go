package account

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNickname(t *testing.T) {
	tests := []struct {
		name     string
		nickname string
		wantErr  error
	}{
		{"three latin letters", "abc", nil},
		{"single chinese letter", "汉", nil},
		{"chinese name", "汉字测试昵称", nil},
		{"ten runes at the limit", "abcdefghij", nil},
		{"ten chinese runes at the limit", "汉字测试昵称汉字测试", nil},
		{"empty", "", ErrNicknameEmpty},
		{"only spaces", "   ", ErrNicknameEmpty},
		{"only tabs and newlines", "\t\n ", ErrNicknameEmpty},
		{"eleven runes", "abcdefghijk", ErrNicknameLength},
		{"eleven chinese runes", "汉字测试昵称汉字测试汉", ErrNicknameLength},
		{"digits", "abc123", ErrNicknameCharacters},
		{"inner space", "a b", ErrNicknameCharacters},
		{"leading space", " abc", ErrNicknameCharacters},
		{"punctuation", "hello!", ErrNicknameCharacters},
		{"underscore", "a_b", ErrNicknameCharacters},
		{"emoji", "ab😀", ErrNicknameCharacters},
		{"digit only", "1", ErrNicknameCharacters},
		{"mixed japanese kana", "ひらがな", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNickname(tt.nickname)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateNickname(%q) = %v, want %v", tt.nickname, err, tt.wantErr)
			}
		})
	}
}

func TestValidateNicknameCountsRunesNotBytes(t *testing.T) {
	// Ten Chinese runes are 30 bytes: a byte based limit would reject them.
	nickname := "汉字测试昵称汉字测试"
	if len(nickname) <= NicknameMaxRunes {
		t.Fatalf("fixture is only %d bytes, so it cannot prove rune counting", len(nickname))
	}
	if err := ValidateNickname(nickname); err != nil {
		t.Fatalf("ValidateNickname(%q) = %v, want nil", nickname, err)
	}
}

func TestValidateBio(t *testing.T) {
	tests := []struct {
		name    string
		bio     string
		wantErr error
	}{
		{"empty", "", nil},
		{"exactly three hundred runes", strings.Repeat("a", 300), nil},
		{"exactly three hundred chinese runes", strings.Repeat("汉", 300), nil},
		{"one over the limit", strings.Repeat("a", 301), ErrBioLength},
		{"chinese one over the limit", strings.Repeat("汉", 301), ErrBioLength},
		{"newlines and markdown", "line one\nline two\n\n- **bold** and `code`\n", nil},
		{"three hundred runes with newlines", strings.Repeat("a\n", 150), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBio(tt.bio)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateBio(%d runes) = %v, want %v", len([]rune(tt.bio)), err, tt.wantErr)
			}
		})
	}
}

func TestValidateBioCountsRunesNotBytes(t *testing.T) {
	// 300 Chinese runes are 900 bytes, so a byte based check would reject a bio
	// that is well inside the rune budget.
	bio := strings.Repeat("汉", 300)
	if len(bio) <= BioMaxRunes {
		t.Fatalf("fixture is only %d bytes, so it cannot prove rune counting", len(bio))
	}
	if err := ValidateBio(bio); err != nil {
		t.Fatalf("ValidateBio(300 chinese runes) = %v, want nil", err)
	}
}
