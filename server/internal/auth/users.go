package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	ErrUserNotFound  = errors.New("user not found")
	ErrEmailTaken    = errors.New("email already registered")
	ErrAvatarMissing = errors.New("avatar attachment unavailable")
)

// DefaultEmailPreferences is applied when an account is created. Replies and answers to a user's own
// content are the transactional cases users expect; comment traffic stays off until they opt in.
//
// This is the single source of those defaults, and the contract's description of
// `GET /me/notification-preferences` names it. That matters because "no row" and "a new account" are
// different states: a missing row reads as all-false (nobody ever chose anything), while every account
// this function creates does have a row. Changing a value here changes what a brand-new reader receives
// by mail, so it belongs in the same change as the contract text and the delivery tests.
const (
	defaultEmailOnComment = false
	defaultEmailOnAnswer  = true
	defaultEmailOnReply   = true
)

// User is the account projection. It never carries a password hash or an anonymous mapping.
type User struct {
	ID            uint64
	PublicID      string
	Email         string
	Nickname      string
	Bio           string
	Role          string
	CreatedAt     time.Time
	AvatarKey     string
	Banned        bool
	Muted         bool
	MuteExpiresAt *time.Time
}

// AvatarURL returns the controlled media path, or an empty string when no avatar is set. The key is
// random, so the address never carries identity.
func (u User) AvatarURL() string {
	if u.AvatarKey == "" {
		return ""
	}
	return "/media/" + u.AvatarKey
}

// AccountStatus reports the derived status expected by the API model.
func (u User) AccountStatus() string {
	if u.Banned {
		return "banned"
	}
	return "active"
}

// activeRestriction matches only restrictions that are in force right now, so an expired ban stops
// blocking login without anyone editing a status column.
const activeRestriction = `r.revoked_at IS NULL AND r.starts_at<=UTC_TIMESTAMP(6) AND (r.ends_at IS NULL OR r.ends_at>UTC_TIMESTAMP(6))`

const userColumns = `u.id,u.public_id,u.email_normalized,u.nickname,u.bio,u.role,u.created_at,
 COALESCE(a.storage_key,''),
 EXISTS(SELECT 1 FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='ban' AND ` + activeRestriction + `),
 EXISTS(SELECT 1 FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='mute' AND ` + activeRestriction + `),
 (SELECT MAX(r.ends_at) FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='mute' AND ` + activeRestriction + `),
 (SELECT COUNT(*) FROM user_restrictions r WHERE r.user_id=u.id AND r.kind='mute' AND ` + activeRestriction + ` AND r.ends_at IS NULL)`

const userFrom = ` FROM users u LEFT JOIN attachments a ON a.id=u.avatar_attachment_id AND a.status IN ('pending','active')`

type rowScanner interface{ Scan(...any) error }

func scanUser(row rowScanner) (User, error) {
	var u User
	var muteEnds sql.NullTime
	var indefinite int
	err := row.Scan(&u.ID, &u.PublicID, &u.Email, &u.Nickname, &u.Bio, &u.Role, &u.CreatedAt,
		&u.AvatarKey, &u.Banned, &u.Muted, &muteEnds, &indefinite)
	if err != nil {
		return u, err
	}
	// An indefinite mute outranks any dated one, so the API omits the expiry instead of showing a
	// deadline that does not end the restriction.
	if indefinite == 0 && muteEnds.Valid {
		ends := muteEnds.Time
		u.MuteExpiresAt = &ends
	}
	return u, nil
}

// UserByID loads one account. ErrUserNotFound means the row is gone; it is never used to signal a
// permission problem.
func (s Store) UserByID(ctx context.Context, id uint64) (User, error) {
	u, err := scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userColumns+userFrom+` WHERE u.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUserNotFound
	}
	return u, err
}

// UserByEmail loads an account together with its password hash, for login only. The hash is
// returned separately so it cannot be attached to a User and serialised by accident.
func (s Store) UserByEmail(ctx context.Context, normalizedEmail string) (User, string, error) {
	var hash string
	row := s.DB.QueryRowContext(ctx, `SELECT `+userColumns+`,u.password_hash`+userFrom+` WHERE u.email_normalized=?`, normalizedEmail)
	var u User
	var muteEnds sql.NullTime
	var indefinite int
	err := row.Scan(&u.ID, &u.PublicID, &u.Email, &u.Nickname, &u.Bio, &u.Role, &u.CreatedAt,
		&u.AvatarKey, &u.Banned, &u.Muted, &muteEnds, &indefinite, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return u, "", ErrUserNotFound
	}
	if err != nil {
		return u, "", err
	}
	if indefinite == 0 && muteEnds.Valid {
		ends := muteEnds.Time
		u.MuteExpiresAt = &ends
	}
	return u, hash, nil
}

// NewUser is the validated input for account creation. The caller normalises the email and hashes
// the password first: this layer must never see a plaintext password.
type NewUser struct {
	Email        string
	PasswordHash string
	Nickname     string
	Bio          string
	Role         string
	PublicID     string
}

// CreateUser inserts the account and its default notification preferences in one transaction, so a
// registered user can always read a complete preference set. A duplicate email surfaces as
// ErrEmailTaken from the unique index rather than from a pre-check, which keeps concurrent
// registrations from both succeeding.
func (s Store) CreateUser(ctx context.Context, in NewUser) (uint64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	role := in.Role
	if role == "" {
		role = "user"
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO users
 (public_id,email_normalized,password_hash,nickname,bio,role) VALUES (?,?,?,?,?,?)`,
		in.PublicID, in.Email, in.PasswordHash, in.Nickname, in.Bio, role)
	if err != nil {
		var mysqlErr *mysql.MySQLError
		if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
			return 0, ErrEmailTaken
		}
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_preferences
 (user_id,email_on_comment,email_on_answer,email_on_reply) VALUES (?,?,?,?)`,
		id, defaultEmailOnComment, defaultEmailOnAnswer, defaultEmailOnReply); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint64(id), nil
}

// ProfilePatch carries only the submitted profile fields; a nil field is left unchanged.
type ProfilePatch struct {
	Nickname           *string
	Bio                *string
	AvatarAttachmentID *uint64
}

// Empty reports whether the patch would change nothing.
func (p ProfilePatch) Empty() bool {
	return p.Nickname == nil && p.Bio == nil && p.AvatarAttachmentID == nil
}

// UpdateProfile applies the patch and returns the refreshed account. An avatar is accepted only when
// the attachment exists, is not deleted and belongs to the caller; otherwise the request is rejected
// rather than silently ignored, and the response does not reveal whether the id exists.
func (s Store) UpdateProfile(ctx context.Context, userID uint64, patch ProfilePatch) (User, error) {
	if patch.Empty() {
		return User{}, &ValidationError{Field: "patch"}
	}
	if patch.AvatarAttachmentID != nil {
		var exists int
		err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM attachments
 WHERE id=? AND owner_id=? AND status IN ('pending','active')`, *patch.AvatarAttachmentID, userID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrAvatarMissing
		}
		if err != nil {
			return User{}, err
		}
	}
	set := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if patch.Nickname != nil {
		set = append(set, "nickname=?")
		args = append(args, *patch.Nickname)
	}
	if patch.Bio != nil {
		set = append(set, "bio=?")
		args = append(args, *patch.Bio)
	}
	if patch.AvatarAttachmentID != nil {
		set = append(set, "avatar_attachment_id=?")
		args = append(args, *patch.AvatarAttachmentID)
	}
	args = append(args, userID)
	query := "UPDATE users SET " + strings.Join(set, ",") + " WHERE id=?"
	if _, err := s.DB.ExecContext(ctx, query, args...); err != nil {
		return User{}, fmt.Errorf("cannot update profile: %w", err)
	}
	return s.UserByID(ctx, userID)
}

// ValidationError names the rejected request field so the HTTP layer can answer 422 without echoing
// the submitted value.
type ValidationError struct{ Field string }

func (e *ValidationError) Error() string { return "invalid field: " + e.Field }
