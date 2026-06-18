// Package config loads the ingester's environment configuration. Two values
// are required: DATABASE_URL and USER_AGENT. Both fail fast at startup so a
// misconfigured pod never starts polling and writes to nothing (or worse,
// writes nothing to a real DB because the URL was wrong).
//
// The polling intervals have defaults but no overrides — v1 doesn't expose
// them, and constants are easier to reason about than env plumbing for a
// process with one valid cadence.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL string
	UserAgent   string

	MappingInterval time.Duration
	Poll5mInterval  time.Duration
	Poll1mInterval  time.Duration
}

func Load() (Config, error) {
	loadDotenv()

	c := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		UserAgent:       os.Getenv("USER_AGENT"),
		MappingInterval: 24 * time.Hour,
		Poll5mInterval:  5 * time.Minute,
		Poll1mInterval:  1 * time.Minute,
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.UserAgent == "" {
		return c, fmt.Errorf("USER_AGENT is required (Wiki API blocks blank UAs)")
	}
	return c, nil
}

// loadDotenv best-effort loads a .env file for local runs. It walks up from the
// working directory looking for the first .env (so `go run .` from ingester/ or
// the repo root both work) and loads it WITHOUT overriding variables already set
// in the real environment — a deploy that injects DATABASE_URL directly always
// wins over a stale checked-out .env. A missing .env is not an error: in
// production there is no file and the env is set by the orchestrator.
func loadDotenv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		p := filepath.Join(dir, ".env")
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Load(p) // Load (not Overload): real env vars take precedence
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return // reached filesystem root, no .env found
		}
		dir = parent
	}
}
