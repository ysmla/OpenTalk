// Command worker delivers the notification events the API queued in the outbox.
//
// It is a separate process on purpose. Delivery is slow, retried and occasionally broken (SMTP outages),
// and none of that belongs in the request path that publishes an answer: the API commits the event and
// returns, and this process picks it up. Running it separately also means several instances can run at
// once — the lease in the outbox keeps them off each other's events — and that a delivery failure restarts
// nothing but the mail.
//
// Usage:
//
//	worker -once                 # one pass, for cron/Task Scheduler
//	worker -interval 30s         # keep running, one pass every interval
//	worker -action show          # print the resolved configuration and exit
//
// The SMTP configuration comes from the administrator settings, not from the environment, so the worker
// sends with exactly what the settings screen shows.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"opentalk/server/internal/content"
	"opentalk/server/internal/mailer"
	"opentalk/server/internal/platform/config"
	"opentalk/server/internal/platform/database"
	"opentalk/server/internal/platform/secretbox"
	"opentalk/server/internal/settings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误："+err.Error())
		os.Exit(1)
	}
}

func run() error {
	action := flag.String("action", "deliver", "deliver 或 show")
	once := flag.Bool("once", false, "只运行一次投递后退出（适合计划任务）")
	interval := flag.Duration("interval", 30*time.Second, "循环模式下的投递间隔")
	batch := flag.Int("batch", 0, "每次取件上限；0 表示使用默认值")
	dryRun := flag.Bool("dry-run", false, "只统计待发事件，不发送也不改动任何行")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.Open(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	// The worker uses its own connection pool size: it runs a handful of queries per pass and must not
	// compete with the API's pool on the same database.
	db.SetMaxOpenConns(4)

	// Same derivation as cmd/api and cmd/admin, so an unsubscribe link this process mints is one the API
	// accepts.
	settingsKey, err := secretbox.Derive(cfg.AppSecret, "settings-secret")
	if err != nil {
		return err
	}
	unsubscribeKey, err := secretbox.Derive(cfg.AppSecret, "unsubscribe-token")
	if err != nil {
		return err
	}
	settingsStore := &settings.Store{DB: db, Key: settingsKey}
	store := content.Store{DB: db}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if *action == "show" {
		return showConfiguration(context.Background(), store, settingsStore)
	}

	if *dryRun {
		pending, err := store.PendingNotificationCount(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("待投递事件：%d（dry-run，未发送任何邮件）\n", pending)
		return nil
	}

	// A single pass is a normal, first-class way to run this: cron on the server, or a container that exits.
	if *once {
		return deliverOnce(context.Background(), store, settingsStore, unsubscribeKey, cfg.AppOrigin, *batch, logger)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("worker started", "interval", interval.String(), "origin", cfg.AppOrigin)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if err := deliverOnce(ctx, store, settingsStore, unsubscribeKey, cfg.AppOrigin, *batch, logger); err != nil {
			// A failure is logged and the loop continues: a database blip must not end the process, and the
			// next pass will retry the same rows because nothing was marked as delivered.
			logger.Error("delivery pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			logger.Info("worker stopped")
			return nil
		case <-ticker.C:
		}
	}
}

// deliverOnce loads the SMTP configuration and runs one delivery pass.
//
// Nobody is mailed while SMTP is unconfigured: the events stay queued and the pass is skipped, so mailing
// starts by itself once an administrator fills the settings in.
func deliverOnce(ctx context.Context, store content.Store, settingsStore *settings.Store, unsubscribeKey []byte, origin string, batch int, logger *slog.Logger) error {
	current, err := settingsStore.Load(ctx)
	if err != nil {
		return err
	}
	if !current.MailerReady() {
		logger.Warn("smtp is not configured; notification mail stays queued")
		return nil
	}
	sender, err := mailer.New(mailer.Config{
		Host:        current.SMTPHost,
		Port:        current.SMTPPort,
		Security:    current.Security,
		Username:    current.SMTPUsername,
		Password:    current.SMTPPassword,
		FromAddress: current.SMTPFromAddress,
		FromName:    current.SMTPFromName,
	})
	if err != nil {
		return err
	}
	report, err := store.DeliverNotifications(ctx, sender, content.DeliveryOptions{
		Now:            time.Now().UTC(),
		BatchSize:      batch,
		UnsubscribeKey: unsubscribeKey,
		AppOrigin:      origin,
	})
	if err != nil {
		return err
	}
	// One structured line per pass: enough to see progress without logging an address or a subject.
	logger.Info("delivery pass",
		"claimed", report.Claimed,
		"sent", report.Sent,
		"coalesced", report.Coalesced,
		"skipped", report.Skipped,
		"deferred", report.Deferred,
		"failed", report.Failed)
	return nil
}

// showConfiguration prints what a pass would use, so an operator can tell "no mail is going out" from
// "mail is going out with the wrong settings". The SMTP password is never printed, only whether one is set.
func showConfiguration(ctx context.Context, store content.Store, settingsStore *settings.Store) error {
	current, err := settingsStore.Load(ctx)
	if err != nil {
		return err
	}
	pending, err := store.PendingNotificationCount(ctx)
	if err != nil {
		return err
	}
	ready := "否（事件会留在队列里，不会发信）"
	if current.MailerReady() {
		ready = "是"
	}
	password := "未配置"
	if current.SMTPPassword != "" {
		password = "已配置（不回显）"
	}
	fmt.Println("投递 worker 配置")
	fmt.Println("SMTP 就绪：" + ready)
	fmt.Println("SMTP 主机：" + current.SMTPHost)
	fmt.Printf("SMTP 端口：%d\n", current.SMTPPort)
	fmt.Println("加密方式：" + current.Security)
	fmt.Println("发信地址：" + current.SMTPFromAddress)
	fmt.Println("SMTP 密码：" + password)
	fmt.Printf("待投递事件：%d\n", pending)
	fmt.Println("合并窗口：10 分钟；每收件人每小时上限：10 封（已确认参数）")
	return nil
}
