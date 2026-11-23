package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"maillens/internal/config"
	"maillens/internal/engine"
	"maillens/internal/logx"
	"maillens/internal/source/outlook"
	"maillens/internal/store"
	"maillens/internal/store/pg"
	"maillens/internal/web"
)

func engineOptions(cfg *config.Config) engine.Options {
	return engine.Options{
		MirrorDeletions:   cfg.Sync.MirrorDeletions,
		MassDeletePercent: cfg.Sync.MassDeletePercent,
		MassDeleteMin:     cfg.Sync.MassDeleteMin,
		Pause:             time.Duration(cfg.Sync.PauseMS) * time.Millisecond,
		BodyMaxChars:      cfg.Sync.BodyMaxChars,
		BatchSize:         cfg.Sync.BatchSize,
	}
}

// syncOnce runs one synchronisation: it takes the lock, connects to Outlook, and lets the engine do the work.
func syncOnce(ctx context.Context, db *pg.DB, cfg *config.Config, log *slog.Logger, opt engine.Options) (engine.Result, error) {
	release, err := db.Lock(ctx)
	if err != nil {
		return engine.Result{}, err
	}
	defer release()

	src, err := outlook.Open(outlook.Options{
		Stores: cfg.Outlook.Stores, ExcludeFolders: cfg.Outlook.ExcludeFolders, StartIfNeeded: cfg.Outlook.StartIfNeeded,
	}, log)
	if err != nil {
		return engine.Result{}, err
	}
	defer src.Close()
	log.Info("connected to Outlook", "version", src.Version())

	e := &engine.Engine{Src: src, DB: db, Log: log, Opt: opt}
	return e.Run(ctx)
}

func printResult(w io.Writer, r engine.Result, dry bool) {
	prefix := "sync"
	if dry {
		prefix = "dry run (nothing was changed)"
	}
	fmt.Fprintf(w, "%s finished in %s: added %s, updated %s, moved %s, deleted %s, unchanged %s",
		prefix, r.Duration.Round(time.Second), commas(int64(r.Added)), commas(int64(r.Updated)), commas(int64(r.Moved)),
		commas(int64(r.Deleted)), commas(int64(r.Unchanged)))
	if r.Failed > 0 {
		fmt.Fprintf(w, ", unreadable %d", r.Failed)
	}
	if r.Skipped > 0 {
		fmt.Fprintf(w, ", vanished %d", r.Skipped)
	}
	fmt.Fprintln(w)
	if r.ScanErrors > 0 {
		fmt.Fprintf(w, "WARNING: %d folder(s) could not be scanned; nothing was deleted from the affected store (see the log)\n", r.ScanErrors)
	}
	if r.DeletionsBlocked > 0 {
		fmt.Fprintf(w, "WARNING: %d message(s) that are gone from Outlook were not removed from the index (see the log)\n", r.DeletionsBlocked)
	}
}

func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("migrate", stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	ctx, stop := interruptContext()
	defer stop()
	log := logx.Console(cfg.Log.Level)
	db, err := pg.Open(ctx, cfg.Postgres, cfg.Search.MaxCandidates, log)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: PostgreSQL:", explainPG(err, cfg.Postgres))
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintln(stderr, "maillens: migrate:", err)
		return 1
	}
	v, _ := db.SchemaVersion(ctx)
	fmt.Fprintf(stdout, "schema %q is at version %d\n", cfg.Postgres.Schema, v)
	return 0
}

func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("sync", stderr)
	storeFilter := fs.String("store", "", "only stores whose name or key contains this text")
	folderFilter := fs.String("folder", "", "only folders whose path contains this text")
	limit := fs.Int("limit", 0, "read at most this many messages in this run (0 for all); for a first test try -limit 200")
	retry := fs.Bool("retry-failed", false, "read again the messages that could not be read last time")
	allowMass := fs.Bool("allow-mass-delete", false, "remove many messages from the index even though the safeguard refuses (a missing store looks like this)")
	dry := fs.Bool("dry-run", false, "scan and compare, change nothing")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	log, closer, err := logx.New(cfg.Log.Dir, cfg.Log.Level)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: log folder:", err)
		return 1
	}
	defer closer.Close()
	ctx, stop := interruptContext()
	defer stop()

	db, err := pg.Open(ctx, cfg.Postgres, cfg.Search.MaxCandidates, log)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: PostgreSQL:", explainPG(err, cfg.Postgres))
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintln(stderr, "maillens: migrate:", err)
		return 1
	}

	opt := engineOptions(cfg)
	opt.StoreFilter, opt.FolderFilter, opt.Limit = *storeFilter, *folderFilter, *limit
	opt.RetryFailed, opt.AllowMassDelete, opt.DryRun = *retry, *allowMass, *dry

	res, err := syncOnce(ctx, db, cfg, log, opt)
	switch {
	case errors.Is(err, store.ErrLocked):
		fmt.Fprintln(stderr, "maillens:", err, "(another 'maillens sync' or 'maillens run' is working on the same database)")
		return 1
	case err != nil:
		printResult(stdout, res, *dry)
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	printResult(stdout, res, *dry)
	return 0
}

func cmdServe(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("serve", stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	log, closer, err := logx.New(cfg.Log.Dir, cfg.Log.Level)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: log folder:", err)
		return 1
	}
	defer closer.Close()
	ctx, stop := interruptContext()
	defer stop()

	db, err := pg.Open(ctx, cfg.Postgres, cfg.Search.MaxCandidates, log)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: PostgreSQL:", explainPG(err, cfg.Postgres))
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintln(stderr, "maillens: migrate:", err)
		return 1
	}
	fmt.Fprintf(stdout, "search page: http://%s/   (Ctrl+C stops it)\n", cfg.Web.Listen)
	if err := web.Serve(ctx, cfg.Web.Listen, web.New(db, log, cfg.Web.PageSize).Handler(), log); err != nil {
		fmt.Fprintln(stderr, "maillens: web server:", err)
		return 1
	}
	return 0
}

// cmdRun serves the page and keeps the index up to date. When Outlook is not running the page still works and the
// sync tries again a little later.
func cmdRun(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("run", stderr)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	log, closer, err := logx.New(cfg.Log.Dir, cfg.Log.Level)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: log folder:", err)
		return 1
	}
	defer closer.Close()
	ctx, stop := interruptContext()
	defer stop()

	db, err := pg.Open(ctx, cfg.Postgres, cfg.Search.MaxCandidates, log)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: PostgreSQL:", explainPG(err, cfg.Postgres))
		return 1
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		fmt.Fprintln(stderr, "maillens: migrate:", err)
		return 1
	}

	webDone := make(chan error, 1)
	go func() {
		webDone <- web.Serve(ctx, cfg.Web.Listen, web.New(db, log, cfg.Web.PageSize).Handler(), log)
	}()
	fmt.Fprintf(stdout, "search page: http://%s/   syncing every %d minute(s)   (Ctrl+C stops)\n", cfg.Web.Listen, cfg.Sync.IntervalMinutes)

	interval := time.Duration(cfg.Sync.IntervalMinutes) * time.Minute
	retryAfter := min(interval, 2*time.Minute)
	for {
		res, err := syncOnce(ctx, db, cfg, log, engineOptions(cfg))
		wait := interval
		switch {
		case ctx.Err() != nil:
		case errors.Is(err, store.ErrLocked):
			log.Info("another sync is running, trying again later")
		case err != nil:
			log.Error("sync failed, trying again soon", "error", err, "retry_in", retryAfter.String())
			wait = retryAfter
		default:
			log.Info("sync finished", "added", res.Added, "updated", res.Updated, "moved", res.Moved, "deleted", res.Deleted,
				"unchanged", res.Unchanged, "failed", res.Failed, "took", res.Duration.Round(time.Second).String())
		}
		select {
		case <-ctx.Done():
			<-webDone
			return 0
		case err := <-webDone:
			if err != nil {
				fmt.Fprintln(stderr, "maillens: web server:", err)
				return 1
			}
			return 0
		case <-time.After(wait):
		}
	}
}
