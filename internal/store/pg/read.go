package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"maillens/internal/model"
	"maillens/internal/store"
)

// Message returns one stored message with its whole body.
func (d *DB) Message(ctx context.Context, id int64) (model.Message, error) {
	var m model.Message
	var received, modified *time.Time
	var importance int16
	err := d.pool.QueryRow(ctx, d.q(`
		SELECT id, store_key, entry_id, folder_path, internet_message_id, subject, sender_name, sender_email,
			to_names, cc_names, received_at, modified_at, size_bytes, importance, has_attachments,
			attachment_names, body, body_truncated, fetch_error
		FROM {s}.message WHERE id = $1`), id).Scan(
		&m.ID, &m.StoreKey, &m.EntryID, &m.FolderPath, &m.InternetMessageID, &m.Subject, &m.SenderName, &m.SenderEmail,
		&m.ToNames, &m.CcNames, &received, &modified, &m.SizeBytes, &importance, &m.HasAttachments,
		&m.AttachmentNames, &m.Body, &m.BodyTruncated, &m.FetchError)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Message{}, store.ErrNotFound
	}
	if err != nil {
		return model.Message{}, err
	}
	if received != nil {
		m.ReceivedAt = received.UTC()
	}
	if modified != nil {
		m.ModifiedAt = modified.UTC()
	}
	m.Importance = int(importance)
	return m, nil
}

// Status describes the index: how many messages, from when to when, how big, and the last sync run.
func (d *DB) Status(ctx context.Context) (model.Status, error) {
	var s model.Status
	var oldest, newest *time.Time
	if err := d.pool.QueryRow(ctx, d.q(`SELECT count(*), min(received_at), max(received_at) FROM {s}.message`)).Scan(&s.Messages, &oldest, &newest); err != nil {
		return s, err
	}
	if oldest != nil {
		s.Oldest = oldest.UTC()
	}
	if newest != nil {
		s.Newest = newest.UTC()
	}
	if err := d.pool.QueryRow(ctx, `SELECT pg_total_relation_size($1::regclass)`, `"`+d.schema+`".message`).Scan(&s.DBSizeBytes); err != nil {
		return s, err
	}
	if v, err := d.ServerVersion(ctx); err == nil {
		s.ServerVersion = v
	}
	var r model.RunRecord
	err := d.pool.QueryRow(ctx, d.q(`
		SELECT started_at, finished_at, kind, added, updated, moved, deleted, failed, note
		FROM {s}.sync_run ORDER BY id DESC LIMIT 1`)).Scan(
		&r.StartedAt, &r.FinishedAt, &r.Kind, &r.Added, &r.Updated, &r.Moved, &r.Deleted, &r.Failed, &r.Note)
	switch {
	case err == nil:
		r.StartedAt, r.FinishedAt = r.StartedAt.UTC(), r.FinishedAt.UTC()
		s.LastRun = &r
	case errors.Is(err, pgx.ErrNoRows):
	default:
		return s, err
	}
	return s, nil
}

// Folders lists the folders of the index with their message counts.
func (d *DB) Folders(ctx context.Context) ([]model.FolderCount, error) {
	rows, err := d.pool.Query(ctx, d.q(`SELECT folder_path, count(*) FROM {s}.message GROUP BY folder_path ORDER BY folder_path`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FolderCount
	for rows.Next() {
		var f model.FolderCount
		if err := rows.Scan(&f.Path, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
