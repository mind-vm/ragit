package bootstrap

import (
	"bufio"
	"fmt"
	"net/url"
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

	if err := cfg.checkDatabaseName(); err != nil {
		return Config{}, err
	}
	if !isSafeIdentifier(cfg.AppRole) {
		// The role name is interpolated into DDL — PostgreSQL cannot
		// parameterise a role — so it is checked rather than trusted.
		return Config{}, fmt.Errorf("DEMO_APP_ROLE %q is not a plain identifier", cfg.AppRole)
	}
	return cfg, nil
}

// RequireEdenAI reports a missing key with the fix rather than letting the
// first embedding call fail somewhere less obvious.
//
// The suggested command exports one variable rather than sourcing the file.
// Those env files carry a DATABASE_URL of their own, and sourcing one would
// silently repoint everything below at a real project's database — where
// Setup would then create roles and tables.
func (c Config) RequireEdenAI() error {
	if c.EdenAIKey == "" {
		return fmt.Errorf("EDENAI_API_KEY is not set; run:\n" +
			"  export EDENAI_API_KEY=$(grep '^EDENAI_API_KEY=' ~/.config/envs/valiro-go.env | cut -d= -f2-)")
	}
	return nil
}

// ExpectedDatabase is the database these examples are allowed to modify.
const ExpectedDatabase = "ragit_examples"

// AllowAnyDatabaseEnv opts out of that check.
const AllowAnyDatabaseEnv = "RAGIT_EXAMPLES_ALLOW_ANY_DB"

// checkDatabaseName refuses to touch a database that is not obviously this
// example's own.
//
// Setup creates roles and tables. DATABASE_URL is an extremely common variable
// to already have exported — every project env file in ~/.config/envs sets one
// — so the realistic accident is not a typo but a shell that was already
// pointed somewhere real. Failing loudly costs one env var to override and
// saves a migration applied to a production database.
func (c Config) checkDatabaseName() error {
	if os.Getenv(AllowAnyDatabaseEnv) == "1" {
		return nil
	}
	u, err := url.Parse(c.AdminDSN)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name != ExpectedDatabase {
		return fmt.Errorf(
			"DATABASE_URL points at database %q, not %q.\n"+
				"  These examples create roles and tables. If that is genuinely what you want,\n"+
				"  set %s=1. If it is not, you probably sourced a project env file — export\n"+
				"  EDENAI_API_KEY on its own instead of sourcing the whole thing.",
			name, ExpectedDatabase, AllowAnyDatabaseEnv)
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
