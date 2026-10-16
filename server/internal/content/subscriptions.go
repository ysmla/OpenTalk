package content

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"opentalk/server/internal/auth"
	"opentalk/server/internal/platform/httpapi"
)

// Per-topic email overrides, and the unsubscribe link that mail carries.
//
// A topic subscription does not decide *whether* a reader is notified — the notification is written where
// the content is written, and the in-site list is unaffected by this setting. It only overrides the
// account's global email preference for one discussion: `inherit` (follow the global flags), `enabled`
// (mail me about this topic even if the global flags are off) and `disabled` (never mail me about this
// topic). That is why the row is per (user, topic) and why "no row" means inherit rather than "off".

// Email override values, as the contract's `email_override` enum spells them.
const (
	overrideInherit  = "inherit"
	overrideEnabled  = "enabled"
	overrideDisabled = "disabled"
)

func validOverride(value string) bool {
	switch value {
	case overrideInherit, overrideEnabled, overrideDisabled:
		return true
	default:
		return false
	}
}

var (
	// ErrUnsubscribeToken is returned for a token that is malformed, forged, or past its expiry. One error
	// for all three: telling a forger which part failed is a gift.
	ErrUnsubscribeToken = errors.New("unsubscribe token is not valid")
	// ErrSubscriptionMissing is returned when a topic has no row for this reader, which means "inherit".
	ErrSubscriptionMissing = errors.New("no subscription row")
)

// TopicSubscription reads one reader's override for one topic. No row reads as `inherit`.
func (s Store) TopicSubscription(ctx context.Context, userID, topicID uint64) (string, error) {
	var override string
	err := s.DB.QueryRowContext(ctx,
		`SELECT email_override FROM topic_subscriptions WHERE user_id=? AND topic_id=?`, userID, topicID).
		Scan(&override)
	if errors.Is(err, sql.ErrNoRows) {
		return overrideInherit, nil
	}
	if err != nil {
		return "", err
	}
	return override, nil
}

// SaveTopicSubscription upserts one reader's override for one topic.
func (s Store) SaveTopicSubscription(ctx context.Context, userID, topicID uint64, override string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO topic_subscriptions (user_id,topic_id,email_override)
VALUES (?,?,?) AS incoming
ON DUPLICATE KEY UPDATE email_override=incoming.email_override`,
		userID, topicID, override)
	return err
}

// ClearTopicSubscription removes one reader's override for one topic, returning that topic to
// `inherit`.
//
// Deleting is not the same as storing `disabled`, and the difference is visible to the reader on the
// next read: a removed row reads as `inherit`, so the topic follows the global preference again. That
// is what "取消订阅" means here — the reader is withdrawing the per-topic choice, not choosing the
// opposite of it. The unsubscribe link in a mail therefore clears the row: the reader asked to stop
// this topic's mail, and the state they are left in is the one they had before ever setting it.
//
// Removing a row that is not there is a no-op, so a twice-used link succeeds both times.
func (s Store) ClearTopicSubscription(ctx context.Context, userID, topicID uint64) error {
	_, err := s.DB.ExecContext(ctx,
		`DELETE FROM topic_subscriptions WHERE user_id=? AND topic_id=?`, userID, topicID)
	return err
}

// unsubscribeTokenTTL bounds how long a link in an old mail keeps working. The mail may sit in an inbox for
// a while, so the window is generous; it exists so a leaked archive of old mail is not a permanent key.
const unsubscribeTokenTTL = 180 * 24 * time.Hour

// mintUnsubscribeToken builds the signed token an unsubscribe link carries.
//
// The payload names the account and the topic, and nothing else: no purpose that could be swapped, no
// free-form text, no id of the notification that happened to trigger it. The signature covers those bytes,
// so a token cannot be edited into one for another topic or another account. A version prefix is included
// so a future format can be told apart instead of being misread as this one.
func mintUnsubscribeToken(key []byte, userID, topicID uint64, now time.Time) (string, error) {
	if len(key) == 0 {
		return "", errors.New("unsubscribe key is not configured")
	}
	payload := "v1|u" + strconv.FormatUint(userID, 10) +
		"|t" + strconv.FormatUint(topicID, 10) +
		"|e" + strconv.FormatInt(now.Add(unsubscribeTokenTTL).Unix(), 10)
	signature := hmac.New(sha256.New, key)
	signature.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(signature.Sum(nil)), nil
}

// parseUnsubscribeToken verifies the signature and the expiry, and returns what the token authorises.
//
// The comparison is constant-time: a signature check that leaks where two strings first differ can be
// walked one byte at a time. Expiry is inside the signed payload, so it cannot be extended by editing the
// token, and a token minted with a different purpose key fails the signature rather than being interpreted.
func parseUnsubscribeToken(key []byte, token string, now time.Time) (userID, topicID uint64, err error) {
	if len(key) == 0 {
		return 0, 0, ErrUnsubscribeToken
	}
	// Bound the input before doing any work on it: the token is attacker-supplied and arrives unauthenticated.
	if token == "" || len(token) > 512 {
		return 0, 0, ErrUnsubscribeToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return 0, 0, ErrUnsubscribeToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, 0, ErrUnsubscribeToken
	}
	provided, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, 0, ErrUnsubscribeToken
	}
	expected := hmac.New(sha256.New, key)
	expected.Write(payload)
	if subtle.ConstantTimeCompare(provided, expected.Sum(nil)) != 1 {
		return 0, 0, ErrUnsubscribeToken
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 4 || fields[0] != "v1" {
		return 0, 0, ErrUnsubscribeToken
	}
	userID, err = parseTokenID(fields[1], "u")
	if err != nil {
		return 0, 0, err
	}
	topicID, err = parseTokenID(fields[2], "t")
	if err != nil {
		return 0, 0, err
	}
	expiry, err := parseTokenID(fields[3], "e")
	if err != nil {
		return 0, 0, err
	}
	if now.Unix() > int64(expiry) {
		return 0, 0, ErrUnsubscribeToken
	}
	return userID, topicID, nil
}

// parseTokenID reads one `<letter><digits>` field of the payload.
func parseTokenID(field, prefix string) (uint64, error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, ErrUnsubscribeToken
	}
	value, err := strconv.ParseUint(field[len(prefix):], 10, 64)
	if err != nil || value == 0 {
		return 0, ErrUnsubscribeToken
	}
	return value, nil
}

// subscriptionResponse mirrors the TopicSubscription schema.
type subscriptionResponse struct {
	TopicID       string `json:"topic_id"`
	EmailOverride string `json:"email_override"`
}

// reachableTopic checks that a topic exists and is reachable before its subscription is read or written.
//
// A hidden or deleted topic is reported as missing, like everywhere else in this package. The consequence
// is deliberate: a reader cannot change their mail setting for a discussion they can no longer open, and
// none of those mails would be sent anyway because delivery re-checks visibility.
func (d Deps) reachableTopic(w http.ResponseWriter, r *http.Request, topicID uint64) bool {
	root, err := loadRootTopic(r.Context(), d.Store.DB, topicID)
	if err != nil {
		d.failRead(w, r, err)
		return false
	}
	if root.Deleted || root.Hidden {
		httpapi.Fail(w, r, 404, "CONTENT_NOT_FOUND", "内容不存在或不可见")
		return false
	}
	return true
}

func (d Deps) getSubscription(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	topicID, ok := pathID(w, r, "topic_id")
	if !ok {
		return
	}
	if !d.reachableTopic(w, r, topicID) {
		return
	}
	override, err := d.Store.TopicSubscription(r.Context(), principal.UserID, topicID)
	if err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, subscriptionResponse{
		TopicID:       strconv.FormatUint(topicID, 10),
		EmailOverride: override,
	})
}

func (d Deps) setSubscription(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.FromContext(r.Context())
	if !ok {
		httpapi.Fail(w, r, 401, "UNAUTHENTICATED", "请先登录")
		return
	}
	topicID, ok := pathID(w, r, "topic_id")
	if !ok {
		return
	}
	var request struct {
		TopicID       string `json:"topic_id"`
		EmailOverride string `json:"email_override"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if request.TopicID == "" {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "必须提供 topic_id")
		return
	}
	bodyTopicID, err := ParseID(request.TopicID)
	if err != nil {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "topic_id 无效")
		return
	}
	// The contract says the body's topic_id must equal the one in the path, so a client cannot believe it
	// changed one topic while changing another.
	if bodyTopicID != topicID {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "topic_id 必须与路径一致")
		return
	}
	if !validOverride(request.EmailOverride) {
		httpapi.Fail(w, r, 422, "INVALID_REQUEST", "email_override 只能是 inherit、enabled 或 disabled")
		return
	}
	if !d.reachableTopic(w, r, topicID) {
		return
	}
	if err := d.Store.SaveTopicSubscription(r.Context(), principal.UserID, topicID, request.EmailOverride); err != nil {
		d.failDependency(w, r, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, subscriptionResponse{
		TopicID:       strconv.FormatUint(topicID, 10),
		EmailOverride: request.EmailOverride,
	})
}

// unsubscribeEmail consumes a token from a notification mail and cancels that one topic's subscription.
//
// It is the only endpoint here that does not require a session, because the whole point is to let somebody
// act on a mail without an account flow. What stands in for authentication is the signature and the scope
// inside it: the token names exactly one account and one topic, so the most a leaked link can do is cancel
// the subscription for the topic it was minted for. It cannot read anything, and it cannot touch the global
// preferences — a reader who wants to stop all mail does that in the settings screen, where the effect is
// visible.
//
// Cancelling clears the per-topic row rather than storing `disabled` (see ClearTopicSubscription): the
// reader is withdrawing their per-topic choice, so the topic goes back to following the global preference.
// That does mean an account whose global reply/answer mail is on will keep receiving mail for this topic
// after unsubscribing — which is exactly what "inherit" promises, and why the settings screen remains the
// place to stop mail altogether.
//
// The route is POST, not GET, and the middleware requires our own Origin: a mail scanner that follows
// links must not be able to unsubscribe somebody by fetching the URL.
func (d Deps) unsubscribeEmail(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	userID, topicID, err := parseUnsubscribeToken(d.UnsubscribeKey, request.Token, time.Now())
	if err != nil {
		httpapi.Fail(w, r, 400, "INVALID_TOKEN", "退订链接无效或已过期")
		return
	}
	if err := d.Store.ClearTopicSubscription(r.Context(), userID, topicID); err != nil {
		d.failDependency(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
