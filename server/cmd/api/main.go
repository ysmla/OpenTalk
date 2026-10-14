package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"opentalk/server/internal/auth"
	"opentalk/server/internal/content"
	"opentalk/server/internal/health"
	"opentalk/server/internal/platform/config"
	"opentalk/server/internal/platform/database"
	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/platform/secretbox"
	"opentalk/server/internal/settings"
	"opentalk/server/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.Open(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	cache := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, MaxRetries: -1})
	defer cache.Close()

	// Each subsystem gets its own subkey derived from APP_SECRET, so a key exposed for one purpose
	// cannot be reused for another.
	csrfKey, err := secretbox.Derive(cfg.AppSecret, "session-csrf")
	if err != nil {
		return err
	}
	challengeKey, err := secretbox.Derive(cfg.AppSecret, "challenge-digest")
	if err != nil {
		return err
	}
	settingsKey, err := secretbox.Derive(cfg.AppSecret, "settings-secret")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	health.Register(mux, func(ctx context.Context) error {
		if err := db.PingContext(ctx); err != nil {
			return err
		}
		var version uint64
		var dirty bool
		if err := db.QueryRowContext(ctx, "SELECT version,dirty FROM schema_migrations").Scan(&version, &dirty); err != nil {
			return err
		}
		if dirty || version != uint64(migrations.Latest) {
			return errors.New("schema version mismatch")
		}
		return nil
	}, func(ctx context.Context) error { return cache.Ping(ctx).Err() })
	// One sessions and one settings instance are shared by the account and content routes, so the two
	// packages cannot drift apart on cookie policy or on the encryption key.
	sessions := auth.Sessions{DB: db, CSRFKey: csrfKey, Secure: cfg.CookieSecure()}
	settingsStore := &settings.Store{DB: db, Key: settingsKey}
	auth.Register(mux, auth.Deps{
		Store:      auth.Store{DB: db},
		Sessions:   sessions,
		Challenges: auth.Challenges{Redis: cache, HashKey: challengeKey},
		Settings:   settingsStore,
		Logger:     logger,
		TrustProxy: cfg.TrustProxy,
	})
	content.Register(mux, content.Deps{
		Store:    content.Store{DB: db},
		Logger:   logger,
		Sessions: sessions,
		Settings: settingsStore,
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { httpapi.Fail(w, r, 404, "NOT_FOUND", "接口不存在") })
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: httpapi.Middleware(cfg.AppOrigin, logger, mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	logger.Info("API listening", "address", cfg.HTTPAddr)
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
	}
	return nil
}
