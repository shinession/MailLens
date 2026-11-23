package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"maillens/internal/logx"
	"maillens/internal/model"
	"maillens/internal/store/pg"
)

func parseDate(s string, addDays int) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date, use YYYY-MM-DD", s)
	}
	return t.AddDate(0, 0, addDays), nil
}

func cmdSearch(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("search", stderr)
	from := fs.String("from", "", "part of the sender name or address")
	since := fs.String("since", "", "received on or after this day (YYYY-MM-DD)")
	until := fs.String("until", "", "received on or before this day (YYYY-MM-DD)")
	folder := fs.String("folder", "", `folder path, for example "name@example.com\Inbox"; includes the folders below it`)
	attach := fs.Bool("attach", false, "only messages with attachments")
	sortBy := fs.String("sort", "relevance", "relevance or date")
	n := fs.Int("n", 20, "results per page")
	page := fs.Int("page", 1, "page number")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	p := model.SearchParams{
		Query: strings.Join(fs.Args(), " "), From: *from, Folder: *folder, Attachments: *attach,
		Sort: *sortBy, Limit: max(1, min(*n, 200)), Offset: (max(1, *page) - 1) * max(1, min(*n, 200)),
	}
	if p.Sort != "date" {
		p.Sort = "relevance"
	}
	if p.Since, err = parseDate(*since, 0); err != nil {
		fmt.Fprintln(stderr, "maillens: -since:", err)
		return 2
	}
	if p.Until, err = parseDate(*until, 1); err != nil {
		fmt.Fprintln(stderr, "maillens: -until:", err)
		return 2
	}

	ctx, stop := interruptContext()
	defer stop()
	db, err := pg.Open(ctx, cfg.Postgres, cfg.Search.MaxCandidates, logx.Console("warn"))
	if err != nil {
		fmt.Fprintln(stderr, "maillens: PostgreSQL:", explainPG(err, cfg.Postgres))
		return 1
	}
	defer db.Close()
	res, err := db.Search(ctx, p)
	if err != nil {
		fmt.Fprintln(stderr, "maillens: search:", err)
		return 1
	}
	for i, h := range res.Hits {
		who := h.SenderName
		if who == "" {
			who = h.SenderEmail
		} else if h.SenderEmail != "" {
			who += " <" + h.SenderEmail + ">"
		}
		subject := h.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		tags := "[" + h.FolderPath + "]"
		if h.HasAttachments {
			tags += " [attachment]"
		}
		fmt.Fprintf(stdout, "%3d. %s  %s\n     %s  %s\n", p.Offset+i+1, localTime(h.ReceivedAt), who, subject, tags)
		if snip := strings.Join(strings.Fields(h.Snippet), " "); snip != "" {
			snip = strings.NewReplacer(model.MarkStart, "*", model.MarkEnd, "*").Replace(snip)
			fmt.Fprintf(stdout, "     %s\n", snip)
		}
		fmt.Fprintln(stdout)
	}
	switch {
	case len(res.Hits) == 0:
		fmt.Fprintln(stdout, "no messages found (words such as \"the\" or \"after\" are ignored by the search)")
	default:
		fmt.Fprintf(stdout, "%d result(s) shown, %d ms", len(res.Hits), res.Took.Milliseconds())
		if res.HasMore {
			fmt.Fprintf(stdout, "; more on page %d (-page %d)", *page+1, *page+1)
		}
		if res.Truncated {
			fmt.Fprint(stdout, "; ranked among the newest matches only: narrow the query or use -sort date")
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs, cfgPath := newFlags("status", stderr)
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
	db, err := pg.Open(ctx, cfg.Postgres, cfg.Search.MaxCandidates, logx.Console("warn"))
	if err != nil {
		fmt.Fprintln(stderr, "maillens: PostgreSQL:", explainPG(err, cfg.Postgres))
		return 1
	}
	defer db.Close()
	v, err := db.SchemaVersion(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	if v == 0 {
		fmt.Fprintf(stdout, "schema %q does not exist yet: run 'maillens migrate' or 'maillens sync'\n", cfg.Postgres.Schema)
		return 0
	}
	st, err := db.Status(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "maillens:", err)
		return 1
	}
	fmt.Fprintf(stdout, "messages  : %s", commas(st.Messages))
	if st.Messages > 0 {
		fmt.Fprintf(stdout, " (received %s to %s)", localTime(st.Oldest)[:10], localTime(st.Newest)[:10])
	}
	fmt.Fprintf(stdout, "\ndatabase  : %s, PostgreSQL %s, schema %q\n", humanBytes(st.DBSizeBytes), st.ServerVersion, cfg.Postgres.Schema)
	if r := st.LastRun; r != nil {
		fmt.Fprintf(stdout, "last sync : %s, took %s: added %d, updated %d, moved %d, deleted %d, unreadable %d\n",
			localTime(r.FinishedAt), r.FinishedAt.Sub(r.StartedAt).Round(time.Second), r.Added, r.Updated, r.Moved, r.Deleted, r.Failed)
		if r.Note != "" {
			fmt.Fprintf(stdout, "note      : %s\n", r.Note)
		}
	} else {
		fmt.Fprintln(stdout, "last sync : never")
	}
	return 0
}
