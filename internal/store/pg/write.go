package pg

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"maillens/internal/model"
	"maillens/internal/store"
)

// cleanText makes a string safe for a text column: PostgreSQL rejects NUL characters and invalid UTF-8. A NUL becomes
// a space, so that the words around it stay separate words.
func cleanText(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", " ")
	}
	return strings.ToValidUTF8(s, "�")
}

// nullTime stores a zero time as NULL.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// Lock takes a lock that keeps two sync runs apart. It lives on its own connection and ends with the release
// function, or with the program.
func (d *DB) Lock(ctx context.Context) (func(), error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, d.schema+"/sync").Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, store.ErrLocked
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, d.schema+"/sync")
		conn.Release()
	}, nil
}

// Known returns the stored messages of a store keyed by the Outlook entry id.
func (d *DB) Known(ctx context.Context, storeKey string) (map[string]model.Known, error) {
	rows, err := d.pool.Query(ctx, d.q(`
		SELECT id, entry_id, folder_path, internet_message_id, size_bytes, modified_at, fetch_error <> ''
		FROM {s}.message WHERE store_key = $1`), storeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]model.Known, 4096)
	for rows.Next() {
		var k model.Known
		var entry string
		var mod *time.Time
		if err := rows.Scan(&k.ID, &entry, &k.FolderPath, &k.InternetMessageID, &k.SizeBytes, &mod, &k.FetchFailed); err != nil {
			return nil, err
		}
		if mod != nil {
			k.ModifiedAt = mod.UTC()
		}
		out[entry] = k
	}
	return out, rows.Err()
}

const upsertSQL = `
INSERT INTO {s}.message (store_key, entry_id, folder_path, internet_message_id, subject, sender_name, sender_email,
	to_names, cc_names, received_at, modified_at, size_bytes, importance, has_attachments, attachment_names,
	body, body_truncated, fetch_error)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
ON CONFLICT (store_key, entry_id) DO UPDATE SET
	folder_path = EXCLUDED.folder_path, internet_message_id = EXCLUDED.internet_message_id,
	subject = EXCLUDED.subject, sender_name = EXCLUDED.sender_name, sender_email = EXCLUDED.sender_email,
	to_names = EXCLUDED.to_names, cc_names = EXCLUDED.cc_names, received_at = EXCLUDED.received_at,
	modified_at = EXCLUDED.modified_at, size_bytes = EXCLUDED.size_bytes, importance = EXCLUDED.importance,
	has_attachments = EXCLUDED.has_attachments, attachment_names = EXCLUDED.attachment_names,
	body = EXCLUDED.body, body_truncated = EXCLUDED.body_truncated, fetch_error = EXCLUDED.fetch_error,
	indexed_at = now()`

// Upsert stores messages in one transaction, replacing the ones with the same store and entry id.
func (d *DB) Upsert(ctx context.Context, msgs []model.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	sql := d.q(upsertSQL)
	b := &pgx.Batch{}
	for _, m := range msgs {
		b.Queue(sql,
			m.StoreKey, m.EntryID, cleanText(m.FolderPath), cleanText(m.InternetMessageID),
			cleanText(m.Subject), cleanText(m.SenderName), cleanText(m.SenderEmail),
			cleanText(m.ToNames), cleanText(m.CcNames), nullTime(m.ReceivedAt), nullTime(m.ModifiedAt),
			m.SizeBytes, int16(m.Importance), m.HasAttachments, cleanText(m.AttachmentNames),
			cleanText(m.Body), m.BodyTruncated, cleanText(m.FetchError))
	}
	return d.runBatch(ctx, b, len(msgs))
}

// UpdateMeta changes the cheap fields of stored messages. The search column is not touched, so nothing is re-indexed.
func (d *DB) UpdateMeta(ctx context.Context, ups []model.MetaUpdate) error {
	if len(ups) == 0 {
		return nil
	}
	sql := d.q(`UPDATE {s}.message SET folder_path = $2, modified_at = $3, importance = $4 WHERE id = $1`)
	b := &pgx.Batch{}
	for _, u := range ups {
		b.Queue(sql, u.ID, cleanText(u.FolderPath), nullTime(u.ModifiedAt), int16(u.Importance))
	}
	return d.runBatch(ctx, b, len(ups))
}

// Move re-points stored messages at the entry id and folder they have after a move in Outlook.
func (d *DB) Move(ctx context.Context, mvs []model.MoveUpdate) error {
	if len(mvs) == 0 {
		return nil
	}
	sql := d.q(`UPDATE {s}.message SET entry_id = $2, folder_path = $3, modified_at = $4 WHERE id = $1`)
	b := &pgx.Batch{}
	for _, m := range mvs {
		b.Queue(sql, m.ID, m.EntryID, cleanText(m.FolderPath), nullTime(m.ModifiedAt))
	}
	return d.runBatch(ctx, b, len(mvs))
}

// Delete removes messages by id.
func (d *DB) Delete(ctx context.Context, ids []int64) error {
	const chunk = 5000
	sql := d.q(`DELETE FROM {s}.message WHERE id = ANY($1)`)
	for len(ids) > 0 {
		n := min(chunk, len(ids))
		if _, err := d.pool.Exec(ctx, sql, ids[:n]); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

// RecordRun adds the summary of a sync run to the log.
func (d *DB) RecordRun(ctx context.Context, r model.RunRecord) error {
	_, err := d.pool.Exec(ctx, d.q(`
		INSERT INTO {s}.sync_run (started_at, finished_at, kind, added, updated, moved, deleted, failed, note)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`),
		r.StartedAt.UTC(), r.FinishedAt.UTC(), r.Kind, r.Added, r.Updated, r.Moved, r.Deleted, r.Failed, cleanText(r.Note))
	return err
}

// runBatch sends the batch (one implicit transaction) and checks every statement.
func (d *DB) runBatch(ctx context.Context, b *pgx.Batch, n int) error {
	br := d.pool.SendBatch(ctx, b)
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}
