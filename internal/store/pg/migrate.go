package pg

import (
	"context"
	"fmt"
	"strconv"
)

// searchExpr is the text search column. The subject weighs most, then the sender, then the recipients and attachment
// names, then the body. A tsvector cannot be larger than 1 MB, so only the first 400,000 characters of a body are
// indexed; the whole body is still stored and shown.
//
// Angle brackets are turned into spaces first: the parser of PostgreSQL reads <ABC123> as an HTML tag and drops it,
// and with it the part number, lot number or error code that mail often puts between brackets. Addresses and web
// addresses in brackets survive either way. The snippet in search.go does the same, so that the match is marked.
const searchExpr = `
	setweight(to_tsvector('english', translate(coalesce(subject, ''), '<>', '  ')), 'A') ||
	setweight(to_tsvector('english', translate(coalesce(sender_name, '') || ' ' || coalesce(sender_email, ''), '<>', '  ')), 'B') ||
	setweight(to_tsvector('english', translate(coalesce(to_names, '') || ' ' || coalesce(cc_names, '') || ' ' || coalesce(attachment_names, ''), '<>', '  ')), 'C') ||
	setweight(to_tsvector('english', translate(left(coalesce(body, ''), 400000), '<>', '  ')), 'D')`

// migrations are applied in order, once. Each entry is a list of statements (the driver runs one statement at a time).
var migrations = [][]string{
	{ // 1: the messages, their search index and the log of the runs
		`CREATE TABLE IF NOT EXISTS {s}.message (
			id                  bigserial   PRIMARY KEY,
			store_key           text        NOT NULL,
			entry_id            text        NOT NULL,
			folder_path         text        NOT NULL,
			internet_message_id text        NOT NULL DEFAULT '',
			subject             text        NOT NULL DEFAULT '',
			sender_name         text        NOT NULL DEFAULT '',
			sender_email        text        NOT NULL DEFAULT '',
			to_names            text        NOT NULL DEFAULT '',
			cc_names            text        NOT NULL DEFAULT '',
			received_at         timestamptz,
			modified_at         timestamptz,
			size_bytes          bigint      NOT NULL DEFAULT 0,
			importance          smallint    NOT NULL DEFAULT 1,
			has_attachments     boolean     NOT NULL DEFAULT false,
			attachment_names    text        NOT NULL DEFAULT '',
			body                text        NOT NULL DEFAULT '',
			body_truncated      boolean     NOT NULL DEFAULT false,
			fetch_error         text        NOT NULL DEFAULT '',
			indexed_at          timestamptz NOT NULL DEFAULT now(),
			search              tsvector GENERATED ALWAYS AS (` + searchExpr + `) STORED,
			CONSTRAINT message_store_entry_key UNIQUE (store_key, entry_id)
		)`,
		`CREATE INDEX IF NOT EXISTS message_search_idx   ON {s}.message USING gin (search)`,
		`CREATE INDEX IF NOT EXISTS message_received_idx ON {s}.message (received_at DESC NULLS LAST, id DESC)`,
		`CREATE INDEX IF NOT EXISTS message_folder_idx   ON {s}.message (folder_path)`,
		`CREATE INDEX IF NOT EXISTS message_msgid_idx    ON {s}.message (internet_message_id) WHERE internet_message_id <> ''`,
		`CREATE TABLE IF NOT EXISTS {s}.sync_run (
			id          bigserial   PRIMARY KEY,
			started_at  timestamptz NOT NULL,
			finished_at timestamptz NOT NULL,
			kind        text        NOT NULL,
			added       integer     NOT NULL DEFAULT 0,
			updated     integer     NOT NULL DEFAULT 0,
			moved       integer     NOT NULL DEFAULT 0,
			deleted     integer     NOT NULL DEFAULT 0,
			failed      integer     NOT NULL DEFAULT 0,
			note        text        NOT NULL DEFAULT ''
		)`,
	},
}

// Migrate creates the schema and the tables, or brings them up to date. It is safe to run again and to run from
// two programs at once.
func (d *DB) Migrate(ctx context.Context) error {
	var vnum string
	if err := d.pool.QueryRow(ctx, `SHOW server_version_num`).Scan(&vnum); err != nil {
		return fmt.Errorf("read the server version: %w", err)
	}
	if n, _ := strconv.Atoi(vnum); n < 120000 {
		return fmt.Errorf("PostgreSQL 12 or newer is required (generated columns), the server reports %s", vnum)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, d.schema+"/migrate"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, d.q(`CREATE SCHEMA IF NOT EXISTS {s}`)); err != nil {
		return fmt.Errorf("create schema %s (the database user needs the CREATE privilege on the database, or the schema must exist): %w", d.schema, err)
	}
	if _, err := tx.Exec(ctx, d.q(`CREATE TABLE IF NOT EXISTS {s}.schema_version (version integer NOT NULL)`)); err != nil {
		return err
	}
	var cur int
	if err := tx.QueryRow(ctx, d.q(`SELECT COALESCE(max(version), 0) FROM {s}.schema_version`)).Scan(&cur); err != nil {
		return err
	}
	for i, stmts := range migrations {
		v := i + 1
		if v <= cur {
			continue
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, d.q(s)); err != nil {
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if _, err := tx.Exec(ctx, d.q(`INSERT INTO {s}.schema_version (version) VALUES ($1)`), v); err != nil {
			return err
		}
		d.log.Info("database migrated", "schema", d.schema, "version", v)
	}
	return tx.Commit(ctx)
}

// SchemaVersion returns the applied schema version, 0 when the schema has not been created yet.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var reg *string
	if err := d.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, `"`+d.schema+`".schema_version`).Scan(&reg); err != nil {
		return 0, err
	}
	if reg == nil {
		return 0, nil
	}
	var v int
	err := d.pool.QueryRow(ctx, d.q(`SELECT COALESCE(max(version), 0) FROM {s}.schema_version`)).Scan(&v)
	return v, err
}

// LatestSchemaVersion is the version a fully migrated database has.
func LatestSchemaVersion() int { return len(migrations) }
