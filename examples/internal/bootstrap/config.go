package bootstrap

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/ragitschema"
)

// Config is everything both examples need to reach their infrastructure.
type Config struct {
	// AdminDSN connects as a superuser. Used to migrate and to create the
	// application role — never to run an example.
	AdminDSN string
	// AppRole/AppPassword name the unprivileged role the examples connect as.
	AppRole     string
	AppPassword string

	XbergURL string

	MinIOEndpoint  string
	MinIOAccessKey string
	MinIOSecretKey string
	MinIOBucket    string

	EdenAIKey     string
	EdenAIBaseURL string
	EdenAIModel   string

	// EmbeddingDim is the width of the vector column. It is a schema
	// property, not a runtime setting: ragit's shipped migrations declare
	// 1536, and anything else needs its own generated set. See
	// docs/examples-plan.md, "The dimension fork".
	EmbeddingDim int
}

// LoadConfig reads .env (if present) and then the environment, which wins.
//
// The EdenAI key is read from the environment only. It is never defaulted and
// never read from a file inside this repo — it lives in ~/.config/envs.
func LoadConfig() (Config, error) {
	loadEnvFile(".env")
	loadEnvFile("../.env")

	cfg := Config{
		AdminDSN:       env("DATABASE_URL", "postgres://postgres:postgres@localhost:5455/ragit_examples?sslmode=disable"),
		AppRole:        env("DEMO_APP_ROLE", "ragit_demo"),
		AppPassword:    env("DEMO_APP_PASSWORD", "ragit_demo_pw"),
		XbergURL:       env("XBERG_URL", "http://localhost:8234"),
		MinIOEndpoint:  env("MINIO_ENDPOINT", "localhost:9200"),
		MinIOAccessKey: env("MINIO_ACCESS_KEY", "minioadmin"),
		MinIOSecretKey: env("MINIO_SECRET_KEY", "minioadmin"),
		MinIOBucket:    env("MINIO_BUCKET", "ragit-examples"),
		EdenAIKey:      os.Getenv("EDENAI_API_KEY"),
		EdenAIBaseURL:  env("EDENAI_BASE_URL", embed.DefaultBaseURL),
		EdenAIModel:    env("EDENAI_EMBEDDING_MODEL", embed.DefaultModel),
	}

	dim, err := strconv.Atoi(env("RAG_EMBEDDING_DIM", strconv.Itoa(ragitschema.DefaultEmbeddingDimension)))
	if err != nil {
		return Config{}, fmt.Errorf("RAG_EMBEDDING_DIM: %w", err)
	}
	cfg.EmbeddingDim = dim

	if !isSafeIdentifier(cfg.AppRole) {
		// The role name is interpolated into DDL — PostgreSQL cannot
		// parameterise a role — so it is checked rather than trusted.
		return Config{}, fmt.Errorf("DEMO_APP_ROLE %q is not a plain identifier", cfg.AppRole)
	}
	return cfg, nil
}

// RequireEdenAI reports a missing key with the fix rather than letting the
// first embedding call fail somewhere less obvious.
func (c Config) RequireEdenAI() error {
	if c.EdenAIKey == "" {
		return fmt.Errorf("EDENAI_API_KEY is not set; run: set -a; . ~/.config/envs/valiro-go.env; set +a")
	}
	return nil
}

// AppDSN is AdminDSN with the credentials swapped for the unprivileged role.
// Same host, same database — only the role differs, which is the whole point.
func (c Config) AppDSN() (string, error) {
	return rewriteUser(c.AdminDSN, c.AppRole, c.AppPassword)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadEnvFile sets any KEY=VALUE it finds that is not already in the
// environment. Deliberately minimal — no quoting rules, no interpolation, no
// dependency. A missing file is not an error.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, set := os.LookupEnv(key); set {
			continue
		}
		_ = os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
	}
}

func isSafeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
