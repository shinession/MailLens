// Package pg keeps the index in PostgreSQL: one table of messages with a full text search column, and a log of sync
// runs. Every table lives in the schema named in the settings; the schema name is validated and quoted, all values go
// through query parameters.
package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"maillens/internal/config"
	"maillens/internal/store"
)

var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// DB is the index in one schema of a PostgreSQL database.
type DB struct {
	pool          *pgxpool.Pool
	schema        string
	maxCandidates int
	log           *slog.Logger
}

var (
	_ store.Writer = (*DB)(nil)
	_ store.Reader = (*DB)(nil)
)

// Open connects with the settings. The whole target goes into one connection string, which is parsed as it is.
func Open(ctx context.Context, cfg config.Postgres, maxCandidates int, log *slog.Logger) (*DB, error) {
	return OpenDSN(ctx, cfg.DSN(), cfg.Schema, maxCandidates, log)
}

// OpenDSN connects with a connection string; the tests use it.
func OpenDSN(ctx context.Context, dsn, schema string, maxCandidates int, log *slog.Logger) (*DB, error) {
	if !identRe.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, scrub(err, dsn)
	}
	pc.MaxConns = 6
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, scrub(err, dsn)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, scrub(err, dsn)
	}
	if maxCandidates < 100 {
		maxCandidates = 5000
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &DB{pool: pool, schema: schema, maxCandidates: maxCandidates, log: log}, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Close closes the connections.
func (d *DB) Close() { d.pool.Close() }

// q puts the quoted schema name into a statement: {s} becomes "maillens".
func (d *DB) q(sql string) string {
	return strings.ReplaceAll(sql, "{s}", `"`+d.schema+`"`)
}

// ServerVersion returns the PostgreSQL version, for example "16.4".
func (d *DB) ServerVersion(ctx context.Context) (string, error) {
	var v string
	err := d.pool.QueryRow(ctx, `SHOW server_version`).Scan(&v)
	return v, err
}

// scrubbedError keeps the driver error (so errors.As still finds a *pgconn.PgError) but never shows the password.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrub removes the password of the connection string from an error text.
func scrub(err error, dsn string) error {
	msg := err.Error()
	if u, perr := url.Parse(dsn); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			for _, form := range []string{pw, url.QueryEscape(pw), url.PathEscape(pw)} {
				msg = strings.ReplaceAll(msg, form, "***")
			}
		}
	}
	var already *scrubbedError
	if errors.As(err, &already) {
		return err
	}
	return &scrubbedError{msg: msg, err: err}
}
