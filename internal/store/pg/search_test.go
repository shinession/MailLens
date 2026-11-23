package pg

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"maillens/internal/model"
)

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// checkPlaceholders verifies that the placeholders of a statement and its arguments match exactly: PostgreSQL refuses
// a statement that uses a placeholder without an argument, and an argument that no placeholder uses.
func checkPlaceholders(t *testing.T, name, sql string, args []any) {
	t.Helper()
	seen := map[int]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(sql, -1) {
		n, _ := strconv.Atoi(m[1])
		seen[n] = true
		if n < 1 || n > len(args) {
			t.Errorf("%s: $%d has no argument (%d arguments)", name, n, len(args))
		}
	}
	for i := 1; i <= len(args); i++ {
		if !seen[i] {
			t.Errorf("%s: argument $%d is never used in the statement", name, i)
		}
	}
}

func TestEveryShapeOfSearchHasMatchingPlaceholders(t *testing.T) {
	filters := []model.SearchParams{
		{},
		{From: "jane"},
		{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{Folder: `box\Inbox`},
		{Attachments: true},
		{From: "jane", Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), Folder: `box\Inbox`, Attachments: true},
	}
	for _, query := range []string{"", "invoice", `"exact phrase" -lunch`} {
		for _, sortBy := range []string{"", "relevance", "date"} {
			for i, f := range filters {
				p := f
				p.Query, p.Sort, p.Limit, p.Offset = query, sortBy, 50, 100
				sql, args := buildSearchSQL("maillens", p, 5000)
				checkPlaceholders(t, "query="+query+" sort="+sortBy+" filter#"+strconv.Itoa(i), sql, args)
			}
		}
	}
}

func TestSearchShapes(t *testing.T) {
	rel, _ := buildSearchSQL("maillens", model.SearchParams{Query: "invoice", Limit: 50}, 5000)
	if !strings.Contains(rel, "MATERIALIZED") || !strings.Contains(rel, "ts_rank_cd") || !strings.Contains(rel, "ts_headline") {
		t.Errorf("a relevance search ranks a bounded candidate set and builds a snippet:\n%s", rel)
	}
	if !strings.Contains(rel, "translate(left(m.body, 100000), '<>', '  ')") {
		t.Errorf("the snippet is built from the text with the angle brackets turned into spaces, like the index:\n%s", rel)
	}
	date, dateArgs := buildSearchSQL("maillens", model.SearchParams{Query: "invoice", Sort: "date", Limit: 50}, 5000)
	if strings.Contains(date, "ts_rank_cd") || !strings.Contains(date, "ts_headline") {
		t.Errorf("a date search does not rank:\n%s", date)
	}
	// the arguments are the query, the limit, the offset and the snippet options, in the order they were added
	if got := dateArgs[1]; got != 51 {
		t.Errorf("the limit is one more than the page size, got %v", got)
	}
	browse, _ := buildSearchSQL("maillens", model.SearchParams{Limit: 50}, 5000)
	if strings.Contains(browse, "websearch_to_tsquery") || strings.Contains(browse, "ts_rank_cd") {
		t.Errorf("without a query there is no text search:\n%s", browse)
	}
}

func TestSearchTextNeverReachesTheStatement(t *testing.T) {
	evil := `x'; DROP TABLE message; --`
	sql, args := buildSearchSQL("maillens", model.SearchParams{Query: evil, From: evil, Folder: evil, Limit: 10}, 5000)
	if strings.Contains(sql, "DROP") || strings.Contains(sql, evil) {
		t.Errorf("user text must travel as arguments only:\n%s", sql)
	}
	if args[0] != evil {
		t.Errorf("the query is the first argument, got %v", args[0])
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`50%_off!`); got != `50!%!_off!!` {
		t.Errorf("got %q", got)
	}
}

func TestScrubHidesThePassword(t *testing.T) {
	err := scrub(errString(`cannot parse postgres://u:s3cr%40t@host/db: bad; password s3cr@t`), "postgres://u:s3cr%40t@127.0.0.1:5432/db")
	if strings.Contains(err.Error(), "s3cr") {
		t.Errorf("the password leaked: %v", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestCleanText(t *testing.T) {
	if got := cleanText("a\x00b\xffc"); got != "a b�c" {
		t.Errorf("got %q", got)
	}
}
