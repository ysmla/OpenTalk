package account

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// Enforced on rune counts, not bytes, and kept below the storage capacity of the
	// corresponding database columns.
	PasswordMinRunes = 8
	PasswordMaxRunes = 16
	NicknameMaxRunes = 10
	BioMaxRunes      = 300
)

var (
	ErrNicknameEmpty      = errors.New("nickname empty")
	ErrNicknameLength     = errors.New("nickname length")
	ErrNicknameCharacters = errors.New("nickname characters")
	ErrBioLength          = errors.New("bio length")
)

// ValidateNickname reports whether nickname may be stored. Only the emptiness
// probe ignores surrounding whitespace, because a whitespace-only nickname is
// empty in intent; every other check runs on the value as given so the caller
// cannot be told "valid" for a string it is about to store verbatim.
func ValidateNickname(nickname string) error {
	if strings.TrimSpace(nickname) == "" {
		return ErrNicknameEmpty
	}
	if utf8.RuneCountInString(nickname) > NicknameMaxRunes {
		return ErrNicknameLength
	}
	// Letters only. Counting runes rather than classifying bytes is what lets
	// Chinese names through: unicode.IsLetter('汉') is true, while digits,
	// spaces, punctuation and symbols are not letters.
	for _, r := range nickname {
		if !unicode.IsLetter(r) {
			return ErrNicknameCharacters
		}
	}
	return nil
}

// ValidateBio applies the only rule a biography has: a rune budget. Newlines
// and Markdown are intentional content, so they are not restricted here.
func ValidateBio(bio string) error {
	if utf8.RuneCountInString(bio) > BioMaxRunes {
		return ErrBioLength
	}
	return nil
}
