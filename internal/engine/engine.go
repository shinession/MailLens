// Package engine synchronises the index with the mailbox: it scans Outlook, compares the scan with the stored
// messages, reads the text of what is new, and removes what is gone. Outlook and the database are reached through the
// interfaces of internal/source and internal/store, so the logic is tested with fakes.
//
// A run is resumable by construction: everything it writes is durable batch by batch, and the next run compares
// again, so it carries on with what is still missing.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"maillens/internal/model"
	"maillens/internal/source"
	"maillens/internal/store"
)

// Options controls one run.
type Options struct {
	Limit             int           // read at most this many messages in this run, 0 for all
	FolderFilter      string        // only folders whose path contains this text, for a partial run
	StoreFilter       string        // only stores whose key or name contains this text
	RetryFailed       bool          // read again the messages that could not be read last time
	MirrorDeletions   bool          // remove stored messages whose item is gone from Outlook
	MassDeletePercent int           // see massDelete
	MassDeleteMin     int           //
	AllowMassDelete   bool          // apply a mass deletion anyway
	DryRun            bool          // scan and compare, change nothing
	Pause             time.Duration // after every message read, to keep Outlook responsive
	BodyMaxChars      int           // longer bodies are cut
	BatchSize         int           // messages per database write
}

// Result counts what a run did.
type Result struct {
	Added            int // new messages stored
	Updated          int // messages read again, plus messages whose flags or folder were updated
	Moved            int
	Deleted          int
	Failed           int // items that could not be read (stored without a body)
	Skipped          int // items that vanished between the scan and the read
	DeletionsBlocked int // stored messages that were not removed because of a safeguard or the setting
	Unchanged        int
	ScanErrors       int // folders that could not be listed or scanned: the run is incomplete
	Duration         time.Duration
}

func (r *Result) add(o Result) {
	r.ScanErrors += o.ScanErrors
	r.Added += o.Added
	r.Updated += o.Updated
	r.Moved += o.Moved
	r.Deleted += o.Deleted
	r.Failed += o.Failed
	r.Skipped += o.Skipped
	r.DeletionsBlocked += o.DeletionsBlocked
	r.Unchanged += o.Unchanged
}

// Engine runs the synchronisation.
type Engine struct {
	Src source.Source
	DB  store.Writer
	Log *slog.Logger
	Opt Options
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (e *Engine) batchSize() int {
	if e.Opt.BatchSize > 0 {
		return e.Opt.BatchSize
	}
	return 50
}

// Run synchronises every store. It carries on with the next store when one fails, and returns the first error.
func (e *Engine) Run(ctx context.Context) (Result, error) {
	started := time.Now()
	var total Result
	stores, err := e.Src.Stores(ctx)
	if err != nil {
		return total, fmt.Errorf("list the stores: %w", err)
	}
	var firstErr error
	synced := 0
	for _, st := range stores {
		if e.Opt.StoreFilter != "" && !containsFold(st.Key+" "+st.DisplayName, e.Opt.StoreFilter) {
			continue
		}
		synced++
		r, err := e.syncStore(ctx, st)
		total.add(r)
		if err != nil {
			e.log().Error("a store was not fully synchronised", "store", st.Key, "error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", st.Key, err)
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	if synced == 0 && firstErr == nil {
		firstErr = errors.New("no store to index: Outlook has no primary mailbox or attached .pst file that matches the settings (outlook.stores) and the filter")
	}
	total.Duration = time.Since(started)
	if !e.Opt.DryRun {
		e.record(ctx, started, total, firstErr)
	}
	return total, firstErr
}

// record writes the summary of the run, also when the run was interrupted.
func (e *Engine) record(ctx context.Context, started time.Time, r Result, runErr error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	note := ""
	switch {
	case runErr != nil:
		note = shorten(runErr.Error(), 500)
	case r.ScanErrors > 0:
		note = fmt.Sprintf("%d folder(s) could not be scanned; see the log", r.ScanErrors)
	case r.DeletionsBlocked > 0:
		note = fmt.Sprintf("%d message(s) were not removed from the index; see the log", r.DeletionsBlocked)
	}
	err := e.DB.RecordRun(rctx, model.RunRecord{
		StartedAt: started, FinishedAt: time.Now(), Kind: "sync",
		Added: r.Added, Updated: r.Updated, Moved: r.Moved, Deleted: r.Deleted, Failed: r.Failed, Note: note,
	})
	if err != nil {
		e.log().Warn("the run could not be recorded", "error", err)
	}
}

func (e *Engine) syncStore(ctx context.Context, st source.Store) (Result, error) {
	var res Result
	log := e.log().With("store", st.Key)

	folders, ferr := e.Src.Folders(ctx, st)
	partial := false
	if ferr != nil {
		if !errors.Is(ferr, source.ErrPartial) {
			return res, fmt.Errorf("list the folders: %w", ferr)
		}
		partial = true
		res.ScanErrors++
		log.Warn("the list of folders is incomplete: nothing will be deleted from this store in this run", "error", ferr)
	}
	folders = filterFolders(folders, e.Opt.FolderFilter, func(f source.Folder) string { return f.Path })

	// 1. one bulk scan per folder: everything about every item except its text
	current := make(map[string]model.Meta, 1024)
	for _, f := range folders {
		n := 0
		err := e.Src.ListMeta(ctx, f, func(m model.Meta) error {
			current[m.EntryID] = m
			n++
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			partial = true
			res.ScanErrors++
			log.Warn("a folder could not be scanned: nothing will be deleted from this store in this run", "folder", f.Path, "error", err)
			continue
		}
		log.Debug("folder scanned", "folder", f.Path, "items", n)
	}

	// 2. compare with the index
	known, err := e.DB.Known(ctx, st.Key)
	if err != nil {
		return res, fmt.Errorf("read the stored messages: %w", err)
	}
	known = filterKnown(known, e.Opt.FolderFilter)
	p := buildPlan(current, known, e.Opt.RetryFailed)
	res.Unchanged = p.unchanged
	log.Info("scan finished", "folders", len(folders), "items", len(current), "stored", len(known),
		"new_or_changed", len(p.fetch), "updated", len(p.update), "moved", len(p.move), "gone", len(p.remove))
	if e.Opt.DryRun {
		return res, nil
	}

	// 3. cheap changes first
	if err := chunks(p.move, 2000, func(c []model.MoveUpdate) error { return e.DB.Move(ctx, c) }); err != nil {
		return res, fmt.Errorf("store moved messages: %w", err)
	}
	res.Moved = len(p.move)
	if err := chunks(p.update, 2000, func(c []model.MetaUpdate) error { return e.DB.UpdateMeta(ctx, c) }); err != nil {
		return res, fmt.Errorf("update messages: %w", err)
	}
	res.Updated = len(p.update)

	// 4. read the text of what is new, newest first
	if err := e.fetchAll(ctx, st, p.fetch, known, &res, log); err != nil {
		return res, err
	}

	// 5. deletions last, behind the safeguards
	e.removeGone(ctx, p.remove, len(known), partial, &res, log)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, nil
}

func (e *Engine) fetchAll(ctx context.Context, st source.Store, items []model.Meta, known map[string]model.Known, res *Result, log *slog.Logger) error {
	if e.Opt.Limit > 0 && len(items) > e.Opt.Limit {
		log.Info("limit reached: only the newest messages are read in this run", "limit", e.Opt.Limit, "waiting", len(items)-e.Opt.Limit)
		items = items[:e.Opt.Limit]
	}
	if len(items) == 0 {
		return nil
	}
	batch := make([]model.Message, 0, e.batchSize())
	bytes := 0
	counted := make([]bool, 0, e.batchSize()) // true: replaced an existing message, false: a new one
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := e.DB.Upsert(ctx, batch); err != nil {
			return fmt.Errorf("store messages: %w", err)
		}
		for _, replaced := range counted {
			if replaced {
				res.Updated++
			} else {
				res.Added++
			}
		}
		batch, counted, bytes = batch[:0], counted[:0], 0
		return nil
	}

	start := time.Now()
	lastLog := start
	for i, m := range items {
		if err := ctx.Err(); err != nil {
			_ = flush() // what was read is worth keeping
			return err
		}
		msg := model.Message{StoreKey: st.Key, Meta: m}
		c, err := e.Src.Fetch(ctx, st.Key, m.EntryID, m.HasAttachments)
		switch {
		case err == nil:
			msg.Content = c
			msg.Body, msg.BodyTruncated = truncateRunes(c.Body, e.Opt.BodyMaxChars)
		case errors.Is(err, source.ErrGone):
			res.Skipped++
			continue
		case ctx.Err() != nil:
			_ = flush()
			return ctx.Err()
		default:
			// Keep what the scan knows, so that the message can still be found by its subject and is not read again
			// by accident on every run; "sync -retry-failed" reads it again.
			res.Failed++
			msg.FetchError = shorten(err.Error(), 300)
			log.Warn("an item could not be read; it is stored without its text", "folder", m.FolderPath, "error", err)
		}
		_, replaced := known[m.EntryID]
		batch = append(batch, msg)
		counted = append(counted, replaced)
		bytes += len(msg.Body)
		if len(batch) >= e.batchSize() || bytes >= 8<<20 {
			if err := flush(); err != nil {
				return err
			}
		}
		if time.Since(lastLog) >= 30*time.Second {
			done := i + 1
			rate := float64(done) / time.Since(start).Seconds()
			eta := time.Duration(float64(len(items)-done) / rate * float64(time.Second)).Round(time.Second)
			log.Info("progress", "read", done, "of", len(items), "per_second", fmt.Sprintf("%.1f", rate), "eta", eta)
			lastLog = time.Now()
		}
		if e.Opt.Pause > 0 {
			select {
			case <-time.After(e.Opt.Pause):
			case <-ctx.Done():
			}
		}
	}
	return flush()
}

func (e *Engine) removeGone(ctx context.Context, ids []int64, stored int, partial bool, res *Result, log *slog.Logger) {
	n := len(ids)
	if n == 0 {
		return
	}
	switch {
	case !e.Opt.MirrorDeletions:
		log.Info("messages that are gone from Outlook stay in the index (mirror_deletions is off)", "count", n)
		res.DeletionsBlocked = n
	case partial:
		log.Warn("deletions skipped because the scan of this store was incomplete", "count", n)
		res.DeletionsBlocked = n
	case !e.Opt.AllowMassDelete && massDelete(n, stored, e.Opt.MassDeletePercent, e.Opt.MassDeleteMin):
		log.Error("refusing to remove this many messages from the index: a store or folder may be missing from Outlook; "+
			"if the removal is intended, run: maillens sync -allow-mass-delete",
			"count", n, "stored", stored, "percent", fmt.Sprintf("%.0f", float64(n)*100/float64(stored)))
		res.DeletionsBlocked = n
	default:
		if err := chunks(ids, 5000, func(c []int64) error { return e.DB.Delete(ctx, c) }); err != nil {
			log.Error("messages could not be removed", "error", err)
			res.DeletionsBlocked = n
			return
		}
		res.Deleted = n
	}
}
