package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

func Open(dsn string) (*sql.DB, error) {
	return open(dsn, false)
}

// OpenMigration permits multiple statements only for embedded, reviewed SQL files.
func OpenMigration(dsn string) (*sql.DB, error) {
	return open(dsn, true)
}

func open(dsn string, migration bool) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid MYSQL_DSN")
	}
	if cfg.DBName == "" {
		return nil, fmt.Errorf("MYSQL_DSN must select a database")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = migration
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	if migration {
		cfg.ReadTimeout = 2 * time.Minute
		cfg.WriteTimeout = 2 * time.Minute
	}
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL configuration")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(3 * time.Minute)
	return db, nil
}
