// Command migrate applies, rolls back or reports the gascurve schema
// migrations: `migrate up`, `migrate down N`, `migrate version`.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: migrate up | down N | version")
	}
	url := os.Getenv("DB_URL")
	if url == "" {
		cfg, err := config.LoadForAPI()
		if err != nil {
			return fmt.Errorf("DB_URL not set and config unreadable: %w", err)
		}
		url = cfg.Database.URL
	}
	pool, err := db.Open(url, 2, 1)
	if err != nil {
		return err
	}
	defer pool.Close()
	m, err := db.NewMigrator(pool.DB)
	if err != nil {
		return err
	}
	switch args[0] {
	case "up":
		if err := m.Up(); err != nil {
			return err
		}
		fmt.Fprintln(out, "migrations applied")
	case "down":
		if len(args) < 2 {
			return fmt.Errorf("usage: migrate down N")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n <= 0 {
			return fmt.Errorf("n must be a positive integer")
		}
		if err := m.Down(n); err != nil {
			return err
		}
		fmt.Fprintf(out, "rolled back %d migration(s)\n", n)
	case "version":
		v, dirty, err := m.Version()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "version %d dirty=%v\n", v, dirty)
	default:
		return fmt.Errorf("unknown command %q (expected up, down N or version)", args[0])
	}
	return nil
}
