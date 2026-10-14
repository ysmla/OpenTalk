package config

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
)

// Config carries deployment facts and secrets that must come from the environment. Operational
// limits an administrator may change live in the database instead, so they are deliberately not
// part of this struct.
type Config struct {
	HTTPAddr      string
	AppOrigin     string
	MySQLDSN      string
	RedisAddr     string
	RedisPassword string
	// AppSecret seeds every derived key: the CSRF token binding and the encryption of stored SMTP
	// credentials. Rotating it invalidates existing CSRF tokens and makes stored secrets unreadable.
	AppSecret []byte
	// TrustProxy makes the per-IP limits read X-Forwarded-For. It must stay false unless a reverse
	// proxy that overwrites the header sits in front, because a direct client can forge it.
	TrustProxy bool
	// MediaRoot is the directory uploaded images are written to. It must be a location the process can
	// write and that is excluded from version control.
	MediaRoot string
}

// CookieSecure reports whether session cookies must carry the Secure attribute. It follows the
// configured origin so a local http origin keeps working without weakening a production origin.
func (c Config) CookieSecure() bool {
	u, err := url.Parse(c.AppOrigin)
	return err == nil && u.Scheme == "https"
}

func Load() (Config, error) {
	c := Config{HTTPAddr: os.Getenv("HTTP_ADDR"), AppOrigin: os.Getenv("APP_ORIGIN"), MySQLDSN: os.Getenv("MYSQL_DSN"), RedisAddr: os.Getenv("REDIS_ADDR"), RedisPassword: os.Getenv("REDIS_PASSWORD"), MediaRoot: os.Getenv("MEDIA_ROOT")}
	if c.MediaRoot == "" {
		c.MediaRoot = "data/media"
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return c, fmt.Errorf("HTTP_ADDR must be host:port")
	}
	if c.MySQLDSN == "" {
		return c, fmt.Errorf("MYSQL_DSN is required")
	}
	if _, _, err := net.SplitHostPort(c.RedisAddr); err != nil {
		return c, fmt.Errorf("REDIS_ADDR must be host:port")
	}
	u, err := url.Parse(c.AppOrigin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("APP_ORIGIN must be an http(s) origin without path")
	}
	// Refuse to start without a usable secret rather than falling back to a default one: a shared
	// fallback would silently make every deployment's CSRF binding forgeable.
	secret, err := base64.StdEncoding.DecodeString(os.Getenv("APP_SECRET"))
	if err != nil {
		return c, fmt.Errorf("APP_SECRET must be base64-encoded")
	}
	if len(secret) < 32 {
		return c, fmt.Errorf("APP_SECRET must decode to at least 32 bytes")
	}
	c.AppSecret = secret
	switch os.Getenv("TRUST_PROXY") {
	case "", "false", "0":
		c.TrustProxy = false
	case "true", "1":
		c.TrustProxy = true
	default:
		return c, fmt.Errorf("TRUST_PROXY must be true or false")
	}
	return c, nil
}
