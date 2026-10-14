package migrations

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migrationmysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Creates and drops ONLY a newly generated opentalk_test_* database.
// The supplied DSN needs CREATE DATABASE permission on a disposable local server.
func TestMySQLMigrationConstraints(t *testing.T) {
	dsn := os.Getenv("OPENTALK_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set OPENTALK_TEST_MYSQL_DSN to run real MySQL integration checks")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	cfg.MultiStatements = false
	cfg.Timeout = 5 * time.Second
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("opentalk_test_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec("DROP DATABASE " + name); err != nil {
			t.Errorf("test database cleanup: %v", err)
		}
	}()
	cfg.DBName = name
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := iofs.New(Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	md, err := migrationmysql.WithInstance(db, &migrationmysql.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := migrate.NewWithInstance("iofs", source, "mysql", md)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if err = runner.Up(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=?", name).Scan(&count); err != nil || count != 24 {
		t.Fatalf("table count=%d, err=%v", count, err)
	}
	must := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	reject := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err == nil {
			t.Fatalf("constraint accepted invalid data: %s", q)
		}
	}
	must("INSERT INTO users(public_id,email_normalized,password_hash,nickname,bio) VALUES ('u1','one@example.test','hash','same',''),('u2','two@example.test','hash','same','')")
	reject("INSERT INTO users(public_id,email_normalized,password_hash,nickname,bio) VALUES ('u3','one@example.test','hash','same','')")
	must("INSERT INTO boards(name,description,kind) VALUES ('qa','','qa')")
	must("INSERT INTO contents(author_id,kind,body_md,is_anonymous) VALUES (1,'topic','question',1),(2,'answer','answer',0),(2,'comment','comment',0)")
	must("INSERT INTO topics(content_id,board_id,title,last_activity_at) VALUES (1,1,'title',UTC_TIMESTAMP(6))")
	must("INSERT INTO questions(topic_id) VALUES (1)")
	reject("UPDATE questions SET resolution_status='solved' WHERE topic_id=1")
	must("UPDATE questions SET resolution_status='solved',resolved_at=UTC_TIMESTAMP(6) WHERE topic_id=1")
	must("INSERT INTO answers(content_id,question_id) VALUES(2,1)")
	must("INSERT INTO comments(content_id,topic_id,answer_id) VALUES(3,1,2)")
	must("INSERT INTO anonymous_identities(topic_id,user_id,display_number) VALUES(1,1,1)")
	reject("INSERT INTO anonymous_identities(topic_id,user_id,display_number) VALUES(1,2,1)")
	reject("UPDATE contents SET anonymous_identity_id=1 WHERE id=2")
	must("UPDATE contents SET anonymous_identity_id=1 WHERE id=1")
	must("INSERT INTO reports(reporter_id,content_id,reason,details) VALUES (2,1,'test','')")
	reject("INSERT INTO reports(reporter_id,content_id,reason,details) VALUES (2,1,'duplicate','')")
	must("UPDATE reports SET status='resolved',resolved_at=UTC_TIMESTAMP(6) WHERE id=1")
	must("INSERT INTO reports(reporter_id,content_id,reason,details) VALUES (2,1,'new','')")
	reject("UPDATE contents SET deleted_at=UTC_TIMESTAMP(6) WHERE id=3")
	reject("INSERT INTO deleted_content_archives(content_id,original_body_md,deleted_by,deletion_root_id,deleted_at,purge_after) VALUES(3,'text',1,1,'2026-10-14','2026-10-15')")
	// 000002: settings is a single row with bounded values, and SMTP starts unconfigured.
	var settingsRows, captchaTTL, emailLimit int
	if err = db.QueryRow("SELECT COUNT(*) FROM settings").Scan(&settingsRows); err != nil || settingsRows != 1 {
		t.Fatalf("settings rows=%d, err=%v", settingsRows, err)
	}
	if err = db.QueryRow("SELECT captcha_ttl_seconds,email_code_email_hourly_limit FROM settings WHERE id=1").Scan(&captchaTTL, &emailLimit); err != nil {
		t.Fatal(err)
	}
	if captchaTTL != 300 || emailLimit != 5 {
		t.Fatalf("unexpected settings defaults: captcha_ttl=%d email_limit=%d", captchaTTL, emailLimit)
	}
	var smtpHost *string
	if err = db.QueryRow("SELECT smtp_host FROM settings WHERE id=1").Scan(&smtpHost); err != nil {
		t.Fatal(err)
	}
	if smtpHost != nil {
		t.Fatalf("SMTP must start unconfigured, got host=%q", *smtpHost)
	}
	reject("INSERT INTO settings (id) VALUES (2)")
	reject("UPDATE settings SET captcha_ttl_seconds=10 WHERE id=1")
	reject("UPDATE settings SET login_captcha_failure_threshold=0 WHERE id=1")
	reject("UPDATE settings SET email_code_ip_hourly_limit=5000 WHERE id=1")
	must("UPDATE settings SET smtp_host='smtp.example.test',smtp_port=587 WHERE id=1")
	// 000003: image limits and cleanup windows share the same row and are bounded in SQL too.
	var mediaBytes, mediaDimension, mediaPerContent, pendingTTL, orphanDays int
	if err = db.QueryRow(`SELECT media_max_bytes,media_max_dimension,media_max_per_content,
 media_pending_ttl_hours,media_orphan_cleanup_days FROM settings WHERE id=1`).
		Scan(&mediaBytes, &mediaDimension, &mediaPerContent, &pendingTTL, &orphanDays); err != nil {
		t.Fatal(err)
	}
	if mediaBytes != 5242880 || mediaDimension != 4096 || mediaPerContent != 9 || pendingTTL != 24 || orphanDays != 7 {
		t.Fatalf("unexpected media defaults: bytes=%d dimension=%d per_content=%d pending_ttl=%d orphan_days=%d",
			mediaBytes, mediaDimension, mediaPerContent, pendingTTL, orphanDays)
	}
	reject("UPDATE settings SET media_max_bytes=1024 WHERE id=1")
	reject("UPDATE settings SET media_max_bytes=209715200 WHERE id=1")
	reject("UPDATE settings SET media_max_dimension=9000 WHERE id=1")
	reject("UPDATE settings SET media_max_per_content=0 WHERE id=1")
	reject("UPDATE settings SET media_pending_ttl_hours=0 WHERE id=1")
	reject("UPDATE settings SET media_orphan_cleanup_days=365 WHERE id=1")
	must("UPDATE settings SET media_max_bytes=2097152,media_max_dimension=2048,media_max_per_content=5 WHERE id=1")
	if err = runner.Down(); err != nil {
		t.Fatal(err)
	}
	if err = runner.Up(); err != nil {
		t.Fatal(err)
	}
	version, dirty, err := runner.Version()
	if err != nil || version != Latest || dirty {
		t.Fatalf("version=%d dirty=%v error=%v", version, dirty, err)
	}
	if !strings.HasPrefix(name, "opentalk_test_") {
		t.Fatal("unexpected test schema")
	}
}
