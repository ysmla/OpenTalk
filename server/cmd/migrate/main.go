package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"opentalk/server/internal/platform/database"
	"opentalk/server/migrations"
)

func main() {
	action := flag.String("action", "up", "up, version, or down-one")
	confirm := flag.Bool("allow-destructive", false, "allow a one-version rollback on a disposable development database")
	flag.Parse()
	if err := run(*action, *confirm); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(action string, allow bool) error {
	if action != "up" && action != "version" && action != "down-one" {
		return errors.New("unknown action")
	}
	if action == "down-one" && !allow {
		return errors.New("down-one requires -allow-destructive; never use on a live forum")
	}
	db, err := database.OpenMigration(os.Getenv("MYSQL_DSN"))
	if err != nil {
		return err
	}
	defer db.Close()
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return err
	}
	driver, err := mysql.WithInstance(db, &mysql.Config{})
	if err != nil {
		return fmt.Errorf("cannot connect migration database: %w", err)
	}
	runner, err := migrate.NewWithInstance("iofs", source, "mysql", driver)
	if err != nil {
		return err
	}
	defer runner.Close()
	switch action {
	case "up":
		err = runner.Up()
	case "down-one":
		err = runner.Steps(-1)
	case "version":
		version, dirty, e := runner.Version()
		if e != nil {
			return e
		}
		fmt.Printf("version=%d dirty=%v\n", version, dirty)
	}
	if errors.Is(err, migrate.ErrNoChange) {
		fmt.Println("no pending migrations")
		return nil
	}
	return err
}
