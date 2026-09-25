// Package testsupport provides shared test infrastructure.
package testsupport

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	_ "github.com/lib/pq"
)

// EnvDSN is the environment variable naming the Postgres instance tests use.
const EnvDSN = "AGENTORCH_TEST_DSN"

var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,48}$`)

// SchemaDSN returns a DSN scoped to its own Postgres schema, creating the
// schema if necessary.
//
// Why this exists: `go test ./...` runs packages CONCURRENTLY. Several test
// packages here need Postgres, and each starts by truncating its tables. Run in
// parallel against one schema they delete each other's fixtures, producing
// failures that look like race conditions in the code under test and that
// vanish when you run the package on its own - the worst kind of flake.
//
// The fix is isolation rather than serialisation: each package gets a private
// schema through the connection's search_path, so they can run in parallel and
// still start from a clean slate. `go test -p 1` would also work, but it hides
// the problem instead of fixing it and makes the suite slower for everyone.
//
// Returns "" when EnvDSN is unset, which callers treat as "skip the Postgres
// half of this test".
func SchemaDSN(schema string) (string, error) {
	base := os.Getenv(EnvDSN)
	if base == "" {
		return "", nil
	}
	if !schemaName.MatchString(schema) {
		// Interpolated into DDL below, so this is a real injection guard and
		// not just tidiness.
		return "", fmt.Errorf("testsupport: invalid schema name %q", schema)
	}

	admin, err := sql.Open("postgres", base)
	if err != nil {
		return "", fmt.Errorf("testsupport: open admin connection: %w", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + schema); err != nil {
		return "", fmt.Errorf("testsupport: create schema %s: %w", schema, err)
	}

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	// libpq passes `options` through to the server, so every pooled connection
	// lands in this schema without the caller having to remember to SET it.
	return base + sep + "options=" + url.QueryEscape("-c search_path="+schema), nil
}

// TruncateAll empties every table in the connection's current schema.
func TruncateAll(db *sql.DB) error {
	_, err := db.Exec(`TRUNCATE audit_log, tool_calls, events, runs, agent_definitions, tenants CASCADE`)
	if err != nil {
		return fmt.Errorf("testsupport: truncate: %w", err)
	}
	return nil
}
