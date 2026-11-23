package pg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"maillens/internal/model"
	"maillens/internal/store"
)

// These tests need a SCRATCH PostgreSQL. Set MAILLENS_TEST_PG to its connection string, for example
//
//	postgres://postgres@127.0.0.1:55439/postgres?sslmode=disable
//
// They create the schema maillens_it in that database and DROP it before and after. Never point them at a database
// whose data you care about.
const itSchema = "maillens_it"

func openIT(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("MAILLENS_TEST_PG")
	if dsn == "" {
		t.Skip("MAILLENS_TEST_PG is not set: the PostgreSQL tests are skipped")
	}
	ctx := context.Background()
	d, err := OpenDSN(ctx, dsn, itSchema, 5000, nil)
	if err != nil {
		t.Fatal(err)
	}
	drop := func() { _, _ = d.pool.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+itSchema+` CASCADE`) }
	drop()
	t.Cleanup(func() { drop(); d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return d
}

const testStore = "mailbox:test"

func msg(entry, folder, subject, body string, received time.Time) model.Message {
	return model.Message{
		StoreKey: testStore,
		Meta: model.Meta{
			EntryID: entry, FolderPath: folder, Subject: subject, ReceivedAt: received, ModifiedAt: received,
			SizeBytes: int64(len(body)), Importance: 1, InternetMessageID: "<" + entry + "@test>",
		},
		Content: model.Content{Body: body},
	}
}

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 9, 0, 0, 0, time.UTC) }

func ids(hits []model.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Subject
	}
	return out
}

func seed(t *testing.T, d *DB) {
	t.Helper()
	a := msg("A", `box\Inbox`, "Invoice 4711 for November", "Please find the invoice attached. Payment due in 30 days.", day(11, 20))
	a.SenderName, a.SenderEmail, a.HasAttachments, a.AttachmentNames = "Jane Doe", "jane@example.com", true, "invoice_4711.pdf"
	b := msg("B", `box\Inbox\Social`, "Lunch", "Do you want to grab lunch? The invoice can wait.", day(11, 21))
	b.SenderName, b.SenderEmail = "Bob", "bob@corp.example"
	c := msg("C", `archive\Inbox`, "Weekly status", "No financial news this week.", day(10, 1))
	e := msg("D", `box\100%_done`, "Special folder", "A folder whose name has LIKE characters.", day(9, 1))
	if err := d.Upsert(context.Background(), []model.Message{a, b, c, e}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	d := openIT(t)
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("a second migration must be a no-op: %v", err)
	}
	v, err := d.SchemaVersion(ctx)
	if err != nil || v != LatestSchemaVersion() {
		t.Errorf("schema version %d, err %v, want %d", v, err, LatestSchemaVersion())
	}
}

func TestSearchRanksFiltersAndHighlights(t *testing.T) {
	d := openIT(t)
	seed(t, d)
	ctx := context.Background()
	search := func(p model.SearchParams) model.SearchResult {
		t.Helper()
		if p.Limit == 0 {
			p.Limit = 50
		}
		r, err := d.Search(ctx, p)
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		return r
	}

	r := search(model.SearchParams{Query: "invoice"})
	if got := ids(r.Hits); len(got) != 2 || got[0] != "Invoice 4711 for November" || got[1] != "Lunch" {
		t.Errorf("relevance: a subject match must outrank a body match, got %v", got)
	}
	if !strings.Contains(r.Hits[0].Snippet, model.MarkStart) || !strings.Contains(r.Hits[0].Snippet, model.MarkEnd) {
		t.Errorf("the snippet must mark the match: %q", r.Hits[0].Snippet)
	}
	if r.Truncated {
		t.Error("two candidates are not a truncated set")
	}

	if got := ids(search(model.SearchParams{Query: "invoice", Sort: "date"}).Hits); len(got) != 2 || got[0] != "Lunch" {
		t.Errorf("date order: newest first, got %v", got)
	}
	if got := ids(search(model.SearchParams{Query: "attaching"}).Hits); len(got) != 1 || got[0] != "Invoice 4711 for November" {
		t.Errorf("stemming: attaching must find attached, got %v", got)
	}
	if got := ids(search(model.SearchParams{Query: `"payment due"`}).Hits); len(got) != 1 {
		t.Errorf("phrase: got %v", got)
	}
	if got := ids(search(model.SearchParams{Query: "invoice -lunch"}).Hits); len(got) != 1 || got[0] != "Invoice 4711 for November" {
		t.Errorf("exclusion: got %v", got)
	}
	if got := ids(search(model.SearchParams{Query: "invoice_4711"}).Hits); len(got) != 1 {
		t.Errorf("an attachment file name must be searchable: got %v", got)
	}
	if got := ids(search(model.SearchParams{Query: "jane"}).Hits); len(got) != 1 {
		t.Errorf("the sender name must be searchable: got %v", got)
	}
	if got := ids(search(model.SearchParams{Query: "zebra"}).Hits); len(got) != 0 {
		t.Errorf("no match, no hits: got %v", got)
	}

	// filters, with and without a query
	if got := ids(search(model.SearchParams{From: "JANE"}).Hits); len(got) != 1 || got[0] != "Invoice 4711 for November" {
		t.Errorf("from is case insensitive: %v", got)
	}
	if got := ids(search(model.SearchParams{Since: day(11, 21)}).Hits); len(got) != 1 || got[0] != "Lunch" {
		t.Errorf("since: %v", got)
	}
	if got := ids(search(model.SearchParams{Until: day(11, 21)}).Hits); len(got) != 3 {
		t.Errorf("until is exclusive: %v", got)
	}
	if got := ids(search(model.SearchParams{Folder: `box\Inbox`}).Hits); len(got) != 2 {
		t.Errorf("a folder includes the folders below it: %v", got)
	}
	if got := ids(search(model.SearchParams{Folder: `box\Inbox\Social`}).Hits); len(got) != 1 || got[0] != "Lunch" {
		t.Errorf("a subfolder alone: %v", got)
	}
	if got := ids(search(model.SearchParams{Folder: `box\100%_done`}).Hits); len(got) != 1 || got[0] != "Special folder" {
		t.Errorf("LIKE characters in a folder name must be literal: %v", got)
	}
	if got := ids(search(model.SearchParams{Folder: `box\100`}).Hits); len(got) != 0 {
		t.Errorf("a name prefix is not a folder: %v", got)
	}
	if got := ids(search(model.SearchParams{Attachments: true}).Hits); len(got) != 1 {
		t.Errorf("attachments only: %v", got)
	}
	if got := ids(search(model.SearchParams{Query: "invoice", Folder: `archive\Inbox`}).Hits); len(got) != 0 {
		t.Errorf("query and folder together: %v", got)
	}
	browse := search(model.SearchParams{})
	if got := ids(browse.Hits); len(got) != 4 || got[0] != "Lunch" || got[3] != "Special folder" {
		t.Errorf("browse without a query is newest first: %v", got)
	}

	// paging
	page1 := search(model.SearchParams{Limit: 3})
	page2 := search(model.SearchParams{Limit: 3, Offset: 3})
	if len(page1.Hits) != 3 || !page1.HasMore || len(page2.Hits) != 1 || page2.HasMore {
		t.Errorf("paging: %d/%v and %d/%v", len(page1.Hits), page1.HasMore, len(page2.Hits), page2.HasMore)
	}
}

func TestRelevanceIsBoundedAndSaysSo(t *testing.T) {
	d := openIT(t)
	d.maxCandidates = 100
	var msgs []model.Message
	for i := 0; i < 150; i++ {
		msgs = append(msgs, msg(fmt.Sprintf("E%03d", i), `box\Inbox`, fmt.Sprintf("Report %d", i), "quarterly numbers", day(11, 1).Add(time.Duration(i)*time.Minute)))
	}
	if err := d.Upsert(context.Background(), msgs); err != nil {
		t.Fatal(err)
	}
	r, err := d.Search(context.Background(), model.SearchParams{Query: "quarterly", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Hits) != 100 {
		t.Errorf("relevance ranks the newest 100 only and says so: truncated=%v hits=%d", r.Truncated, len(r.Hits))
	}
	r, err = d.Search(context.Background(), model.SearchParams{Query: "quarterly", Sort: "date", Limit: 200})
	if err != nil || len(r.Hits) != 150 || r.Truncated {
		t.Errorf("sorting by date is not bounded: hits=%d truncated=%v err=%v", len(r.Hits), r.Truncated, err)
	}
}

func TestKnownMoveUpdateDelete(t *testing.T) {
	d := openIT(t)
	seed(t, d)
	ctx := context.Background()

	known, err := d.Known(ctx, testStore)
	if err != nil || len(known) != 4 {
		t.Fatalf("known: %d entries, err %v", len(known), err)
	}
	a := known["A"]
	if a.ID == 0 || a.FolderPath != `box\Inbox` || a.InternetMessageID != "<A@test>" || a.SizeBytes == 0 || !a.ModifiedAt.Equal(day(11, 20)) || a.FetchFailed {
		t.Errorf("known A: %+v", a)
	}
	if other, _ := d.Known(ctx, "mailbox:someone-else"); len(other) != 0 {
		t.Errorf("another store has no messages: %d", len(other))
	}

	if err := d.UpdateMeta(ctx, []model.MetaUpdate{{ID: a.ID, FolderPath: `box\Inbox`, ModifiedAt: day(12, 1), Importance: 2}}); err != nil {
		t.Fatal(err)
	}
	if m, err := d.Message(ctx, a.ID); err != nil || !m.ModifiedAt.Equal(day(12, 1)) || m.Importance != 2 || m.Subject != "Invoice 4711 for November" {
		t.Errorf("after UpdateMeta: %+v err %v", m, err)
	}

	if err := d.Move(ctx, []model.MoveUpdate{{ID: a.ID, EntryID: "A2", FolderPath: `box\Archive`, ModifiedAt: day(12, 2)}}); err != nil {
		t.Fatal(err)
	}
	known, _ = d.Known(ctx, testStore)
	if _, still := known["A"]; still || known["A2"].ID != a.ID || known["A2"].FolderPath != `box\Archive` {
		t.Errorf("after Move: %+v", known)
	}
	if r, _ := d.Search(ctx, model.SearchParams{Query: "invoice", Folder: `box\Archive`, Limit: 10}); len(r.Hits) != 1 {
		t.Errorf("a moved message is found in its new folder: %v", ids(r.Hits))
	}

	if err := d.Delete(ctx, []int64{a.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Message(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a deleted message is gone, got %v", err)
	}
	if err := d.Delete(ctx, nil); err != nil {
		t.Errorf("deleting nothing is fine: %v", err)
	}
}

func TestUpsertReplacesTheSameEntry(t *testing.T) {
	d := openIT(t)
	ctx := context.Background()
	if err := d.Upsert(ctx, []model.Message{msg("X", `box\Inbox`, "First subject", "alpha text", day(11, 1))}); err != nil {
		t.Fatal(err)
	}
	if err := d.Upsert(ctx, []model.Message{msg("X", `box\Inbox`, "Second subject", "beta text", day(11, 1))}); err != nil {
		t.Fatal(err)
	}
	known, _ := d.Known(ctx, testStore)
	if len(known) != 1 {
		t.Fatalf("same store and entry id is one row, got %d", len(known))
	}
	if r, _ := d.Search(ctx, model.SearchParams{Query: "alpha", Limit: 10}); len(r.Hits) != 0 {
		t.Error("the old text must not be searchable any more")
	}
	if r, _ := d.Search(ctx, model.SearchParams{Query: "beta", Limit: 10}); len(r.Hits) != 1 {
		t.Error("the new text must be searchable")
	}
}

func TestTextThatCouldBreakTheInsert(t *testing.T) {
	d := openIT(t)
	ctx := context.Background()

	// "alpha" and "omega" are not stop words (a word like "after" would be dropped by the english configuration)
	nul := msg("N1", `box\Inbox`, "Has a NUL\x00 in it", "alpha\x00omega \xff\xfe broken bytes", day(11, 1))

	// 3 MB of distinct words: a tsvector over all of it would exceed the 1 MB limit of the type.
	var sb strings.Builder
	sb.WriteString("firstword ")
	for i := 0; sb.Len() < 3_000_000; i++ {
		fmt.Fprintf(&sb, "tok%07d ", i)
	}
	huge := msg("H1", `box\Inbox`, "", sb.String(), day(11, 2))
	huge.BodyTruncated = false

	if err := d.Upsert(ctx, []model.Message{nul, huge}); err != nil {
		t.Fatalf("such text must be stored: %v", err)
	}
	if r, _ := d.Search(ctx, model.SearchParams{Query: "firstword", Limit: 10}); len(r.Hits) != 1 {
		t.Errorf("the start of a huge body is searchable: %v", ids(r.Hits))
	}
	if r, _ := d.Search(ctx, model.SearchParams{Query: "omega", Limit: 10}); len(r.Hits) != 1 {
		t.Errorf("a NUL separates words instead of gluing them together: %v", ids(r.Hits))
	}
	known, _ := d.Known(ctx, testStore)
	m, err := d.Message(ctx, known["H1"].ID)
	if err != nil || len(m.Body) != sb.Len() {
		t.Errorf("the whole body is stored even though only the start is indexed: %d of %d, err %v", len(m.Body), sb.Len(), err)
	}
}

// The parser of PostgreSQL reads <ABC123> as an HTML tag and drops it. Mail puts part numbers, lot numbers and error
// codes between angle brackets, so they must stay searchable (the index turns the brackets into spaces).
func TestIdentifiersInAngleBracketsAreSearchable(t *testing.T) {
	d := openIT(t)
	ctx := context.Background()
	body := "From: Jane <jane@example.com>\nLot <ABC123> failed inspection. Model <GRM155R71C104KA88D> shipped.\n" +
		"Result: <<<ERROR>>> in step 4. Type <Enter> to continue. literal <script>alert(1)</script> text"
	if err := d.Upsert(ctx, []model.Message{msg("B1", `box\Inbox`, "Inspection <LOT77> report", body, day(11, 3))}); err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"abc123", "ABC123", "grm155r71c104ka88d", "error", "enter", "alert", "lot77", "jane@example.com"} {
		r, err := d.Search(ctx, model.SearchParams{Query: word, Limit: 10})
		if err != nil || len(r.Hits) != 1 {
			t.Errorf("%q must be found: %d hits, err %v", word, len(r.Hits), err)
			continue
		}
		if word == "abc123" && !strings.Contains(strings.ToLower(r.Hits[0].Snippet), model.MarkStart+"abc123"+model.MarkEnd) {
			t.Errorf("the match inside the brackets is marked in the snippet: %q", r.Hits[0].Snippet)
		}
	}
	m, err := d.Message(ctx, 1)
	if err != nil || !strings.Contains(m.Body, "<ABC123>") {
		t.Errorf("the stored body keeps the brackets: %v", err)
	}
}

func TestFailedFetchIsRemembered(t *testing.T) {
	d := openIT(t)
	ctx := context.Background()
	m := msg("F1", `box\Inbox`, "Unreadable item", "", day(11, 1))
	m.FetchError = "the item could not be read"
	if err := d.Upsert(ctx, []model.Message{m}); err != nil {
		t.Fatal(err)
	}
	known, _ := d.Known(ctx, testStore)
	if !known["F1"].FetchFailed {
		t.Error("a failed fetch must be flagged, so that the next run does not read it again by accident")
	}
	if r, _ := d.Search(ctx, model.SearchParams{Query: "unreadable", Limit: 10}); len(r.Hits) != 1 {
		t.Error("its subject is still searchable")
	}
}

func TestLockKeepsTwoRunsApart(t *testing.T) {
	d := openIT(t)
	ctx := context.Background()
	release, err := d.Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Lock(ctx); !errors.Is(err, store.ErrLocked) {
		t.Errorf("the second run must be refused, got %v", err)
	}
	release()
	release2, err := d.Lock(ctx)
	if err != nil {
		t.Fatalf("the lock is free again: %v", err)
	}
	release2()
}

func TestStatusFoldersAndRunLog(t *testing.T) {
	d := openIT(t)
	seed(t, d)
	ctx := context.Background()

	st, err := d.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Messages != 4 || !st.Oldest.Equal(day(9, 1)) || !st.Newest.Equal(day(11, 21)) || st.DBSizeBytes <= 0 || st.LastRun != nil || st.ServerVersion == "" {
		t.Errorf("status: %+v", st)
	}
	run := model.RunRecord{StartedAt: day(11, 22), FinishedAt: day(11, 22).Add(time.Minute), Kind: "sync", Added: 4, Deleted: 1, Note: "ok"}
	if err := d.RecordRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	st, _ = d.Status(ctx)
	if st.LastRun == nil || st.LastRun.Added != 4 || st.LastRun.Deleted != 1 || st.LastRun.Kind != "sync" || !st.LastRun.FinishedAt.Equal(run.FinishedAt) {
		t.Errorf("last run: %+v", st.LastRun)
	}
	folders, err := d.Folders(ctx)
	if err != nil || len(folders) != 4 {
		t.Fatalf("folders: %v err %v", folders, err)
	}
	for _, f := range folders {
		if f.Count != 1 {
			t.Errorf("%s has %d", f.Path, f.Count)
		}
	}
}
