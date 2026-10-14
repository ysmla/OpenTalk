// Package testdb creates throwaway MySQL schemas for integration tests. Only test files import it,
// so it never reaches the production binary.
package testdb

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migrationmysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"opentalk/server/migrations"
)

// DSNFromEnv returns the administrator DSN used to create throwaway schemas, or an empty string when
// the caller did not configure one. Callers skip rather than fail, so a unit-test run stays green on a
// machine without MySQL.
func DSNFromEnv() string { return os.Getenv("OPENTALK_TEST_MYSQL_DSN") }

// Open creates a randomly named database, applies every embedded migration to it, and returns the
// handle plus a cleanup function that drops the database again. The DSN must allow CREATE DATABASE;
// an existing schema is never touched.
func Open(dsn string) (*sql.DB, func(), error) {
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid OPENTALK_TEST_MYSQL_DSN: %w", err)
	}
	cfg.DBName = ""
	cfg.MultiStatements = false
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, nil, err
	}
	name := fmt.Sprintf("opentalk_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		_ = admin.Close()
		return nil, nil, fmt.Errorf("cannot create the test database: %w", err)
	}
	cfg.DBName = name
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		_, _ = admin.Exec("DROP DATABASE " + name)
		_ = admin.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = db.Close()
		_, _ = admin.Exec("DROP DATABASE " + name)
		_ = admin.Close()
	}
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	driverInstance, err := migrationmysql.WithInstance(db, &migrationmysql.Config{})
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	runner, err := migrate.NewWithInstance("iofs", source, "mysql", driverInstance)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := runner.Up(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("cannot apply migrations: %w", err)
	}
	// The runner is deliberately not closed: golang-migrate's MySQL driver closes the *sql.DB it was
	// handed, and the caller still needs that handle. The cleanup above closes it exactly once.
	return db, cleanup, nil
}
