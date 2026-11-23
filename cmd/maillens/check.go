package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"maillens/internal/config"
	"maillens/internal/logx"
	"maillens/internal/model"
	"maillens/internal/source"
	"maillens/internal/source/outlook"
	"maillens/internal/store/pg"
	"maillens/internal/version"
)

// withTimeout runs fn and gives up waiting after d. A COM call that hangs cannot be interrupted, so the goroutine is
// left behind; "check" ends the process soon after anyway.
func withTimeout[T any](d time.Duration, what string, fn func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(d):
		var zero T
		return zero, fmt.Errorf("%s did not finish within %s", what, d)
	}
}

func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("check", stderr)
	verbose := fs.Bool("v", false, "list the stores and folders that would be indexed")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	fmt.Fprintf(stdout, "maillens %s\n", version.Version)
	row := func(label, status, detail string) {
		fmt.Fprintf(stdout, "%-13s: %-6s %s\n", label, status, detail)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		row("configuration", "FAILED", err.Error())
		return 1
	}
	row("configuration", "ok", cfg.Path)

	ctx, stop := interruptContext()
	defer stop()
	failed := false

	if !checkPostgres(ctx, cfg, row) {
		failed = true
	}
	if !checkOutlook(ctx, cfg, *verbose, stdout, row) {
		failed = true
	}
	if failed {
		return 1
	}
	fmt.Fprintln(stdout, "everything needed for 'maillens sync' is in place")
	return 0
}

func checkPostgres(ctx context.Context, cfg *config.Config, row func(label, status, detail string)) bool {
	p := cfg.Postgres
	db, err := pg.Open(ctx, p, cfg.Search.MaxCandidates, logx.Console("warn"))
	if err != nil {
		row("PostgreSQL", "FAILED", explainPG(err, p))
		return false
	}
	defer db.Close()
	server, _ := db.ServerVersion(ctx)
	where := fmt.Sprintf("%s:%d database %s, server %s", p.Host, p.Port, p.Database, server)
	v, err := db.SchemaVersion(ctx)
	switch {
	case err != nil:
		row("PostgreSQL", "FAILED", where+": "+err.Error())
		return false
	case v == 0:
		row("PostgreSQL", "ok", where+fmt.Sprintf("; schema %q is not created yet, 'maillens migrate' (or the first sync) creates it", p.Schema))
	case v < pg.LatestSchemaVersion():
		row("PostgreSQL", "ok", where+fmt.Sprintf("; schema %q is at version %d, 'maillens migrate' brings it to %d", p.Schema, v, pg.LatestSchemaVersion()))
	default:
		st, err := db.Status(ctx)
		if err != nil {
			row("PostgreSQL", "FAILED", where+": "+err.Error())
			return false
		}
		row("PostgreSQL", "ok", where+fmt.Sprintf("; schema %q, %s messages indexed", p.Schema, commas(st.Messages)))
	}
	return true
}

// checkOutlook connects to Outlook and reads a little: it prints counts and timings, never mail content.
func checkOutlook(ctx context.Context, cfg *config.Config, verbose bool, out io.Writer, row func(label, status, detail string)) bool {
	log := logx.Console("warn")
	opt := outlook.Options{Stores: cfg.Outlook.Stores, ExcludeFolders: cfg.Outlook.ExcludeFolders, StartIfNeeded: cfg.Outlook.StartIfNeeded}
	src, err := withTimeout(30*time.Second, "connecting to Outlook", func() (*outlook.Source, error) { return outlook.Open(opt, log) })
	if err != nil {
		row("Outlook", "FAILED", err.Error())
		return false
	}
	defer src.Close()
	row("Outlook", "ok", "version "+src.Version())

	stores, err := withTimeout(60*time.Second, "listing the stores", func() ([]source.Store, error) { return src.Stores(ctx) })
	if err != nil {
		row("stores", "FAILED", err.Error())
		return false
	}
	if len(stores) == 0 {
		row("stores", "FAILED", "no primary mailbox or attached .pst file matches outlook.stores in the config")
		return false
	}

	var firstFolders []source.Folder
	for _, st := range stores {
		folders, err := withTimeout(5*time.Minute, "listing the folders", func() ([]source.Folder, error) { return src.Folders(ctx, st) })
		switch {
		case err != nil && !errors.Is(err, source.ErrPartial):
			row("store "+st.Kind, "FAILED", err.Error())
			return false
		case err != nil:
			row("store "+st.Kind, "warn", fmt.Sprintf("%d mail folders, but not all could be listed: %v", len(folders), err))
		default:
			row("store "+st.Kind, "ok", fmt.Sprintf("%d mail folders", len(folders)))
		}
		if verbose {
			fmt.Fprintf(out, "    %s\n", st.DisplayName)
			for _, f := range folders {
				fmt.Fprintf(out, "      %s\n", f.Path)
			}
		}
		firstFolders = append(firstFolders, folders...)
	}

	return readTest(ctx, src, firstFolders, row)
}

// readTest scans the first folder that has mail and reads one body, to find out how fast Outlook answers and whether
// its security prompt gets in the way. It prints numbers only.
func readTest(ctx context.Context, src *outlook.Source, folders []source.Folder, row func(label, status, detail string)) bool {
	for i, f := range folders {
		if i >= 8 {
			break
		}
		var metas []model.Meta
		start := time.Now()
		_, err := withTimeout(2*time.Minute, "scanning a folder", func() (struct{}, error) {
			return struct{}{}, src.ListMeta(ctx, f, func(m model.Meta) error {
				metas = append(metas, m)
				if len(metas) >= 50 {
					return source.ErrStop
				}
				return nil
			})
		})
		if err != nil {
			row("read test", "FAILED", "scanning a folder: "+err.Error())
			return false
		}
		if len(metas) == 0 {
			continue
		}
		scan := time.Since(start)

		start = time.Now()
		c, err := withTimeout(20*time.Second, "reading a message", func() (model.Content, error) {
			return src.Fetch(ctx, f.StoreKey, metas[0].EntryID, metas[0].HasAttachments)
		})
		if err != nil {
			hint := ""
			if errors.Is(err, context.DeadlineExceeded) || err.Error() == "reading a message did not finish within 20s" {
				hint = ". Outlook may be showing a security prompt (programmatic access): answer it, or check File > Options > Trust Center > Trust Center Settings > Programmatic Access"
			}
			row("read test", "FAILED", "reading a message: "+err.Error()+hint)
			return false
		}
		row("read test", "ok", fmt.Sprintf("%d rows scanned in %d ms; one message read in %d ms (%s characters)",
			len(metas), scan.Milliseconds(), time.Since(start).Milliseconds(), commas(int64(len([]rune(c.Body))))))
		return true
	}
	row("read test", "warn", "the first folders are empty, nothing was read")
	return true
}
