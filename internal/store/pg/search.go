package pg

import (
	"context"
	"strconv"
	"strings"
	"time"

	"maillens/internal/model"
)

// headlineOpts makes ts_headline wrap every match in the control characters model.MarkStart and model.MarkEnd; the
// web page turns them into <mark> after escaping the text.
const headlineOpts = "StartSel=" + model.MarkStart + ",StopSel=" + model.MarkEnd +
	`,MaxWords=40,MinWords=15,MaxFragments=2,FragmentDelimiter=" ... "`

// escapeLike escapes the characters that mean something in a LIKE pattern, with "!" as the escape character.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	return strings.ReplaceAll(s, "_", "!_")
}

// buildSearchSQL builds the statement of one search and its arguments. Three shapes:
//
//   - a query sorted by relevance: the newest maxCand matches are ranked (ranking needs the whole text search column
//     of every candidate, so it is bounded), the best page is returned with a snippet;
//   - a query sorted by date: the matches are paged straight from the date index;
//   - no query: the newest messages that fit the filters.
//
// Every placeholder that appears in the statement has an argument and the other way round.
func buildSearchSQL(schema string, p model.SearchParams, maxCand int) (string, []any) {
	tbl := `"` + schema + `".message`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	hasQuery := strings.TrimSpace(p.Query) != ""
	tsq := ""
	var conds []string
	if hasQuery {
		tsq = "websearch_to_tsquery('english', " + arg(p.Query) + ")"
		conds = append(conds, "m.search @@ "+tsq)
	}
	if f := strings.TrimSpace(p.From); f != "" {
		a := arg("%" + escapeLike(f) + "%")
		conds = append(conds, "(m.sender_email ILIKE "+a+" ESCAPE '!' OR m.sender_name ILIKE "+a+" ESCAPE '!')")
	}
	if !p.Since.IsZero() {
		conds = append(conds, "m.received_at >= "+arg(p.Since.UTC()))
	}
	if !p.Until.IsZero() {
		conds = append(conds, "m.received_at < "+arg(p.Until.UTC()))
	}
	if f := strings.TrimSpace(p.Folder); f != "" {
		exact := arg(f)
		below := arg(escapeLike(f) + `\%`)
		conds = append(conds, "(m.folder_path = "+exact+" OR m.folder_path LIKE "+below+" ESCAPE '!')")
	}
	if p.Attachments {
		conds = append(conds, "m.has_attachments")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	const cols = `m.id, m.subject, m.sender_name, m.sender_email, m.received_at, m.folder_path, m.has_attachments, m.size_bytes, m.entry_id`
	const byDate = `m.received_at DESC NULLS LAST, m.id DESC`
	limit := arg(p.Limit + 1) // one more than the page, to know whether there is a next page
	offset := arg(p.Offset)

	switch {
	case hasQuery && p.Sort != "date":
		capacity := arg(maxCand)
		opts := arg(headlineOpts)
		return `WITH cand AS MATERIALIZED (
	SELECT m.id FROM ` + tbl + ` m` + where + ` ORDER BY ` + byDate + ` LIMIT ` + capacity + `
), ranked AS (
	SELECT m.id, ts_rank_cd(m.search, ` + tsq + `, 1) AS rank
	FROM cand c JOIN ` + tbl + ` m ON m.id = c.id
	ORDER BY rank DESC, m.id DESC LIMIT ` + limit + ` OFFSET ` + offset + `
)
SELECT ` + cols + `, ts_headline('english', translate(left(m.body, 100000), '<>', '  '), ` + tsq + `, ` + opts + `) AS snippet,
	r.rank, (SELECT count(*) FROM cand) AS candidates
FROM ranked r JOIN ` + tbl + ` m ON m.id = r.id
ORDER BY r.rank DESC, m.id DESC`, args

	case hasQuery:
		opts := arg(headlineOpts)
		return `WITH page AS (
	SELECT m.id FROM ` + tbl + ` m` + where + ` ORDER BY ` + byDate + ` LIMIT ` + limit + ` OFFSET ` + offset + `
)
SELECT ` + cols + `, ts_headline('english', translate(left(m.body, 100000), '<>', '  '), ` + tsq + `, ` + opts + `) AS snippet,
	0::real AS rank, 0::bigint AS candidates
FROM page p JOIN ` + tbl + ` m ON m.id = p.id
ORDER BY ` + byDate, args

	default:
		return `SELECT ` + cols + `, left(m.body, 300) AS snippet, 0::real AS rank, 0::bigint AS candidates
FROM ` + tbl + ` m` + where + ` ORDER BY ` + byDate + ` LIMIT ` + limit + ` OFFSET ` + offset, args
	}
}

// Search runs one search.
func (d *DB) Search(ctx context.Context, p model.SearchParams) (model.SearchResult, error) {
	start := time.Now()
	if p.Limit < 1 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	sql, args := buildSearchSQL(d.schema, p, d.maxCandidates)
	rows, err := d.pool.Query(ctx, sql, args...)
	if err != nil {
		return model.SearchResult{}, err
	}
	defer rows.Close()

	var res model.SearchResult
	var candidates int64
	for rows.Next() {
		var h model.Hit
		var received *time.Time
		var rank float32
		if err := rows.Scan(&h.ID, &h.Subject, &h.SenderName, &h.SenderEmail, &received, &h.FolderPath,
			&h.HasAttachments, &h.SizeBytes, &h.EntryID, &h.Snippet, &rank, &candidates); err != nil {
			return model.SearchResult{}, err
		}
		if received != nil {
			h.ReceivedAt = received.UTC()
		}
		h.Rank = float64(rank)
		res.Hits = append(res.Hits, h)
	}
	if err := rows.Err(); err != nil {
		return model.SearchResult{}, err
	}
	if len(res.Hits) > p.Limit {
		res.Hits = res.Hits[:p.Limit]
		res.HasMore = true
	}
	res.Truncated = candidates >= int64(d.maxCandidates)
	res.Took = time.Since(start)
	return res, nil
}
