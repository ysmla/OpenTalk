package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"opentalk/server/internal/captcha"
)

var (
	ErrChallengeUnknown = errors.New("challenge unknown or expired")
	ErrChallengePurpose = errors.New("challenge purpose mismatch")
	ErrChallengeWrong   = errors.New("challenge answer incorrect")
	ErrRateLimited      = errors.New("rate limited")
)

// loginFailureWindow is how long a login failure keeps counting towards the captcha threshold. It is
// a constant rather than a setting because an administrator changing it could silently disable the
// brute-force brake between two requests.
const loginFailureWindow = time.Hour

// Challenges keeps short-lived verification state. Only HMAC digests of answers and codes are
// stored, so neither a Redis dump nor a key name reveals a usable value or an email address.
type Challenges struct {
	Redis   *redis.Client
	HashKey []byte
}

// digest binds a value to its purpose, so a digest from one flow cannot be replayed in another.
func (c Challenges) digest(kind, value string) string {
	mac := hmac.New(sha256.New, c.HashKey)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// opaque shortens a digest into a key fragment. Emails never appear verbatim in Redis key names.
func (c Challenges) opaque(kind, value string) string { return c.digest(kind, value)[:22] }

func (c Challenges) captchaKey(id, part string) string { return "captcha:" + id + ":" + part }
func (c Challenges) emailKey(id, part string) string   { return "emailcode:" + id + ":" + part }

// consumeCaptcha atomically compares the submitted digest, then either burns the challenge or
// decrements the remaining attempts. Doing this in Lua keeps two concurrent submissions of the same
// answer from both succeeding, and makes a consumed challenge unreplayable because the keys are gone.
var consumeCaptcha = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return -1 end
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return -2 end
if redis.call('GET', KEYS[1]) == ARGV[2] then
  redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4])
  return 1
end
if redis.call('DECR', KEYS[3]) <= 0 then
  redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4])
end
return 0
`)

// consumeEmailCode additionally binds the code to the submitted email and purpose, so a code issued
// for one address cannot be spent on another.
var consumeEmailCode = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return -1 end
if redis.call('GET', KEYS[3]) ~= ARGV[1] then return -2 end
if redis.call('GET', KEYS[2]) ~= ARGV[2] then return -3 end
if redis.call('GET', KEYS[1]) == ARGV[3] then
  redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4])
  return 1
end
if redis.call('DECR', KEYS[4]) <= 0 then
  redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4])
end
return 0
`)

// CaptchaChallenge is a created challenge. ExpiresAt is computed from the configured TTL so the
// client can display a countdown without trusting its own clock arithmetic.
type CaptchaChallenge struct {
	ID        string
	ExpiresAt time.Time
}

// CreateCaptcha renders an image for a fresh answer and stores the digest plus the image. The answer
// itself is never written anywhere, so it cannot leak from storage.
func (c Challenges) CreateCaptcha(ctx context.Context, purpose string, ttlSeconds, maxAttempts int) (CaptchaChallenge, error) {
	id, err := randomID()
	if err != nil {
		return CaptchaChallenge{}, err
	}
	answer, err := randomFromAlphabet(captcha.Alphabet, 4)
	if err != nil {
		return CaptchaChallenge{}, err
	}
	seedBytes := make([]byte, 8)
	if _, err := rand.Read(seedBytes); err != nil {
		return CaptchaChallenge{}, fmt.Errorf("cannot read randomness: %w", err)
	}
	image, err := captcha.RenderPNG(answer, int64(binary.BigEndian.Uint64(seedBytes)))
	if err != nil {
		return CaptchaChallenge{}, fmt.Errorf("cannot render captcha: %w", err)
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	pipe := c.Redis.TxPipeline()
	pipe.Set(ctx, c.captchaKey(id, "a"), c.digest("captcha", answer), ttl)
	pipe.Set(ctx, c.captchaKey(id, "p"), purpose, ttl)
	pipe.Set(ctx, c.captchaKey(id, "n"), strconv.Itoa(maxAttempts), ttl)
	pipe.Set(ctx, c.captchaKey(id, "i"), image, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return CaptchaChallenge{}, fmt.Errorf("cannot store captcha: %w", err)
	}
	return CaptchaChallenge{ID: id, ExpiresAt: time.Now().UTC().Add(ttl)}, nil
}

// CaptchaImage returns the rendered PNG. A missing image means the challenge expired, was consumed,
// or never existed; the caller answers the same way for all three.
func (c Challenges) CaptchaImage(ctx context.Context, id string) ([]byte, error) {
	image, err := c.Redis.Get(ctx, c.captchaKey(id, "i")).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrChallengeUnknown
	}
	if err != nil {
		return nil, err
	}
	return image, nil
}

// VerifyCaptcha consumes one attempt. The comparison is case-insensitive because the alphabet is
// upper-case and users type lower-case.
func (c Challenges) VerifyCaptcha(ctx context.Context, id, purpose, answer string) error {
	submitted := strings.ToUpper(strings.TrimSpace(answer))
	if submitted == "" {
		return ErrChallengeWrong
	}
	result, err := consumeCaptcha.Run(ctx, c.Redis,
		[]string{c.captchaKey(id, "a"), c.captchaKey(id, "p"), c.captchaKey(id, "n"), c.captchaKey(id, "i")},
		purpose, c.digest("captcha", submitted)).Int64()
	if err != nil {
		return fmt.Errorf("cannot verify captcha: %w", err)
	}
	switch result {
	case 1:
		return nil
	case 0:
		return ErrChallengeWrong
	case -2:
		return ErrChallengePurpose
	default:
		return ErrChallengeUnknown
	}
}

// EmailChallenge is a created email challenge. Code stays inside the process: the caller sends it but
// must never log it or return it in a response.
type EmailChallenge struct {
	ID                string
	ExpiresAt         time.Time
	RetryAfterSeconds int
	Code              string
}

// CreateEmailCode stores the digest of a fresh code. An empty code stores an unusable digest: that is
// how a password reset for an unknown address answers identically without ever sending mail.
func (c Challenges) CreateEmailCode(ctx context.Context, email, purpose, code string, ttlSeconds, maxAttempts, retryAfter int) (EmailChallenge, error) {
	id, err := randomID()
	if err != nil {
		return EmailChallenge{}, err
	}
	digest := ""
	if code != "" {
		digest = c.digest("emailcode", code)
	}
	ttl := time.Duration(ttlSeconds) * time.Second
	pipe := c.Redis.TxPipeline()
	pipe.Set(ctx, c.emailKey(id, "h"), digest, ttl)
	pipe.Set(ctx, c.emailKey(id, "e"), c.opaque("email", email), ttl)
	pipe.Set(ctx, c.emailKey(id, "p"), purpose, ttl)
	pipe.Set(ctx, c.emailKey(id, "n"), strconv.Itoa(maxAttempts), ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return EmailChallenge{}, fmt.Errorf("cannot store email code: %w", err)
	}
	return EmailChallenge{ID: id, ExpiresAt: time.Now().UTC().Add(ttl), RetryAfterSeconds: retryAfter, Code: code}, nil
}

// DiscardEmailCode removes a challenge whose delivery failed, so a code that never arrived cannot be
// left behind as a valid secret.
func (c Challenges) DiscardEmailCode(ctx context.Context, id string) error {
	return c.Redis.Del(ctx, c.emailKey(id, "h"), c.emailKey(id, "e"), c.emailKey(id, "p"), c.emailKey(id, "n")).Err()
}

// VerifyEmailCode consumes one attempt and binds the code to the submitted address and purpose.
func (c Challenges) VerifyEmailCode(ctx context.Context, id, email, purpose, code string) error {
	submitted := strings.TrimSpace(code)
	if submitted == "" {
		return ErrChallengeWrong
	}
	result, err := consumeEmailCode.Run(ctx, c.Redis,
		[]string{c.emailKey(id, "h"), c.emailKey(id, "e"), c.emailKey(id, "p"), c.emailKey(id, "n")},
		purpose, c.opaque("email", email), c.digest("emailcode", submitted)).Int64()
	if err != nil {
		return fmt.Errorf("cannot verify email code: %w", err)
	}
	switch result {
	case 1:
		return nil
	case 0:
		return ErrChallengeWrong
	case -2:
		return ErrChallengePurpose
	case -3:
		return ErrChallengeWrong
	default:
		return ErrChallengeUnknown
	}
}

// EmailSendLimits carries the administrator-configured ceilings so this layer needs no settings
// dependency.
type EmailSendLimits struct {
	ResendIntervalSeconds int
	EmailHourlyLimit      int
	IPHourlyLimit         int
}

// ReserveEmailSend applies the resend interval and both hourly ceilings before a message is sent. It
// returns ErrRateLimited with the seconds to wait. Callers must call ReleaseEmailSend when the
// delivery itself fails, so a transient SMTP outage does not lock the user out for the full interval.
func (c Challenges) ReserveEmailSend(ctx context.Context, email, ip string, limits EmailSendLimits) (int, error) {
	interval := time.Duration(limits.ResendIntervalSeconds) * time.Second
	resendKey := "resend:email:" + c.opaque("email", email)
	reserved, err := c.Redis.SetNX(ctx, resendKey, "1", interval).Result()
	if err != nil {
		return 0, err
	}
	if !reserved {
		ttl, err := c.Redis.TTL(ctx, resendKey).Result()
		if err != nil {
			return 0, err
		}
		return int(ttl.Seconds()) + 1, ErrRateLimited
	}

	// Hour buckets keep the counters self-expiring; the key is deleted an hour after the first hit.
	if seconds, over, err := c.bumpHourly(ctx, "rl:email:"+c.opaque("email", email), limits.EmailHourlyLimit); err != nil {
		return 0, err
	} else if over {
		_ = c.Redis.Del(ctx, resendKey).Err()
		return seconds, ErrRateLimited
	}
	if ip != "" {
		if seconds, over, err := c.bumpHourly(ctx, "rl:ip:"+ip, limits.IPHourlyLimit); err != nil {
			return 0, err
		} else if over {
			_ = c.Redis.Del(ctx, resendKey).Err()
			return seconds, ErrRateLimited
		}
	}
	return 0, nil
}

// ReleaseEmailSend undoes a reservation after a failed delivery.
func (c Challenges) ReleaseEmailSend(ctx context.Context, email, ip string) {
	_ = c.Redis.Del(ctx, "resend:email:"+c.opaque("email", email)).Err()
	c.decrementIfPresent(ctx, "rl:email:"+c.opaque("email", email))
	if ip != "" {
		c.decrementIfPresent(ctx, "rl:ip:"+ip)
	}
}

// decrementIfPresent gives the counter back without ever creating one. A bare DECR on a missing key
// would create a counter with no expiry, which would never reset and could keep an address blocked
// for good.
func (c Challenges) decrementIfPresent(ctx context.Context, key string) {
	// TTL reports -2 for a missing key and -1 for one without an expiry; neither should be touched.
	remaining, err := c.Redis.TTL(ctx, key).Result()
	if err != nil || remaining <= 0 {
		return
	}
	if err := c.Redis.Decr(ctx, key).Err(); err != nil {
		return
	}
	// DECR keeps the existing TTL, but re-asserting it costs nothing and covers a lost expiry.
	_ = c.Redis.Expire(ctx, key, remaining).Err()
}

// bumpHourly counts one hit in the current hour and reports the seconds until the window resets.
func (c Challenges) bumpHourly(ctx context.Context, key string, limit int) (int, bool, error) {
	count, err := c.Redis.Incr(ctx, key).Result()
	if err != nil {
		return 0, false, err
	}
	if count == 1 {
		// Expire the counter so a quiet hour does not leave a permanent key behind.
		if err := c.Redis.Expire(ctx, key, time.Hour).Err(); err != nil {
			return 0, false, err
		}
	}
	if count > int64(limit) {
		return secondsUntilNextHour(), true, nil
	}
	return 0, false, nil
}

func secondsUntilNextHour() int {
	now := time.Now().UTC()
	return int(time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC).Add(time.Hour).Sub(now).Seconds()) + 1
}

// LoginFailures reports how many consecutive failures are recorded for an address.
func (c Challenges) LoginFailures(ctx context.Context, email string) (int, error) {
	value, err := c.Redis.Get(ctx, "loginfail:"+c.opaque("email", email)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return value, err
}

// RecordLoginFailure counts one failure and returns the new total. Counting by address, whether or
// not the account exists, keeps the captcha requirement from revealing registration status.
func (c Challenges) RecordLoginFailure(ctx context.Context, email string) (int, error) {
	key := "loginfail:" + c.opaque("email", email)
	count, err := c.Redis.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if count == 1 {
		if err := c.Redis.Expire(ctx, key, loginFailureWindow).Err(); err != nil {
			return 0, err
		}
	}
	return int(count), nil
}

// ClearLoginFailures forgets the failures after a successful login.
func (c Challenges) ClearLoginFailures(ctx context.Context, email string) error {
	return c.Redis.Del(ctx, "loginfail:"+c.opaque("email", email)).Err()
}

// randomID returns 16 random bytes as an unguessable challenge identifier.
func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("cannot read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// randomDigits returns n decimal digits from a cryptographically secure source. Verification codes
// must not be predictable, so math/rand is not an option here.
func randomDigits(n int) (string, error) {
	out := make([]byte, n)
	for i := range out {
		value, err := randomIndex(10)
		if err != nil {
			return "", err
		}
		out[i] = byte('0' + value)
	}
	return string(out), nil
}

// randomFromAlphabet picks n symbols uniformly. Bytes that would bias the modulo are rejected instead
// of folded, so every symbol keeps the same probability.
func randomFromAlphabet(alphabet string, n int) (string, error) {
	out := make([]byte, n)
	for i := range out {
		value, err := randomIndex(len(alphabet))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[value]
	}
	return string(out), nil
}

func randomIndex(size int) (int, error) {
	if size <= 0 || size > 256 {
		return 0, fmt.Errorf("unsupported alphabet size")
	}
	limit := 256 - (256 % size)
	buffer := make([]byte, 1)
	for {
		if _, err := rand.Read(buffer); err != nil {
			return 0, fmt.Errorf("cannot read randomness: %w", err)
		}
		if int(buffer[0]) < limit {
			return int(buffer[0]) % size, nil
		}
	}
}
