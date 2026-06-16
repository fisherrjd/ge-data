package ingest

import (
	"fmt"
	"os"
)

// Config is read from the environment (see docker-compose.yml / k3s Secret).
type Config struct {
	DatabaseURL string // postgres://user:pass@host:5432/db
	UserAgent   string // required by the Wiki API; identify project + contact
}

func loadConfig() (Config, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	ua := os.Getenv("USER_AGENT")
	if ua == "" {
		return Config{}, fmt.Errorf("USER_AGENT is required (Wiki API blocks generic agents)")
	}
	return Config{DatabaseURL: dsn, UserAgent: ua}, nil
}
