package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"maillens/internal/model"
	"maillens/internal/source"
	"maillens/internal/store"
)

// ---- fakes ----

type fakeSource struct {
	stores     []source.Store
	folders    map[string][]source.Folder // by store key
	items      map[string][]model.Meta    // by folder path
	content    map[string]model.Content   // by entry id
	scanErr    map[string]error           // by folder path
	fetchErr   map[string]error           // by entry id
	foldersErr error                      // returned together with the folders
	fetched    []string

	cancelAfter int // cancel the context after this many reads, 0 for never
	cancel      context.CancelFunc
}

func (f *fakeSource) Stores(context.Context) ([]source.Store, error) { return f.stores, nil }
func (f *fakeSource) Folders(_ context.Context, st source.Store) ([]source.Folder, error) {
	return f.folders[st.Key], f.foldersErr
}
func (f *fakeSource) ListMeta(_ context.Context, fo source.Folder, fn func(model.Meta) error) error {
	if err := f.scanErr[fo.Path]; err != nil {
		return err
	}
	for _, m := range f.items[fo.Path] {
		if err := fn(m); err != nil {
			if errors.Is(err, source.ErrStop) {
				return nil
			}
			return err
		}
	}
	return nil
}
func (f *fakeSource) Fetch(_ context.Context, _, entryID string, _ bool) (model.Content, error) {
	f.fetched = append(f.fetched, entryID)
	if f.cancelAfter > 0 && len(f.fetched) >= f.cancelAfter {
		f.cancel()
	}
	if err := f.fetchErr[entryID]; err != nil {
		return model.Content{}, err
	}
	return f.content[entryID], nil
}
func (f *fakeSource) Close() error { return nil }

type fakeRow struct {
	id int64
	m  model.Message
}

type fakeDB struct {
	rows   map[string]*fakeRow // storeKey|entryID
	nextID int64
	runs   []model.RunRecord
	held   bool
}

func newFakeDB() *fakeDB { return &fakeDB{rows: map[string]*fakeRow{}, nextID: 1} }

func rowKey(storeKey, entry string) string { return storeKey + "|" + entry }

func (d *fakeDB) Lock(context.Context) (func(), error) {
	if d.held {
		return nil, store.ErrLocked
	}
	d.held = true
	return func() { d.held = false }, nil
}
func (d *fakeDB) Known(_ context.Context, storeKey string) (map[string]model.Known, error) {
	out := map[string]model.Known{}
	for _, r := range d.rows {
		if r.m.StoreKey == storeKey {
			out[r.m.EntryID] = model.Known{ID: r.id, FolderPath: r.m.FolderPath, InternetMessageID: r.m.InternetMessageID,
				SizeBytes: r.m.SizeBytes, ModifiedAt: r.m.ModifiedAt, FetchFailed: r.m.FetchError != ""}
		}
	}
	return out, nil
}
func (d *fakeDB) Upsert(_ context.Context, msgs []model.Message) error {
	for _, m := range msgs {
		k := rowKey(m.StoreKey, m.EntryID)
		if r, ok := d.rows[k]; ok {
			r.m = m
		} else {
			d.rows[k] = &fakeRow{id: d.nextID, m: m}
			d.nextID++
		}
	}
	return nil
}
func (d *fakeDB) byID(id int64) *fakeRow {
	for _, r := range d.rows {
		if r.id == id {
			return r
		}
	}
	return nil
}
func (d *fakeDB) UpdateMeta(_ context.Context, ups []model.MetaUpdate) error {
	for _, u := range ups {
		r := d.byID(u.ID)
		if r == nil {
			return fmt.Errorf("no row %d", u.ID)
		}
		r.m.FolderPath, r.m.ModifiedAt, r.m.Importance = u.FolderPath, u.ModifiedAt, u.Importance
	}
	return nil
}
func (d *fakeDB) Move(_ context.Context, mvs []model.MoveUpdate) error {
	for _, mv := range mvs {
		r := d.byID(mv.ID)
		if r == nil {
			return fmt.Errorf("no row %d", mv.ID)
		}
		delete(d.rows, rowKey(r.m.StoreKey, r.m.EntryID))
		r.m.EntryID, r.m.FolderPath, r.m.ModifiedAt = mv.EntryID, mv.FolderPath, mv.ModifiedAt
		d.rows[rowKey(r.m.StoreKey, r.m.EntryID)] = r
	}
	return nil
}
func (d *fakeDB) Delete(_ context.Context, ids []int64) error {
	for _, id := range ids {
		if r := d.byID(id); r != nil {
			delete(d.rows, rowKey(r.m.StoreKey, r.m.EntryID))
		}
	}
	return nil
}
func (d *fakeDB) RecordRun(_ context.Context, r model.RunRecord) error {
	d.runs = append(d.runs, r)
	return nil
}

var _ source.Source = (*fakeSource)(nil)
var _ store.Writer = (*fakeDB)(nil)

// ---- a small world ----

const mailbox = "mailbox:me"

func day(n int) time.Time { return time.Date(2026, 11, n, 12, 0, 0, 0, time.UTC) }

type world struct {
	src *fakeSource
	db  *fakeDB
}

func newWorld() *world {
	st := source.Store{Key: mailbox, Kind: "mailbox", DisplayName: "me"}
	return &world{
		src: &fakeSource{
			stores: []source.Store{st}, folders: map[string][]source.Folder{}, items: map[string][]model.Meta{},
			content: map[string]model.Content{}, scanErr: map[string]error{}, fetchErr: map[string]error{},
		},
		db: newFakeDB(),
	}
}

func (w *world) folder(path string) { w.folderIn(mailbox, path) }
func (w *world) folderIn(storeKey, path string) {
	w.src.folders[storeKey] = append(w.src.folders[storeKey], source.Folder{StoreKey: storeKey, Path: path, EntryID: "F:" + path})
}

func (w *world) add(folder, entry, msgID string, size int64, received time.Time, body string) {
	w.src.items[folder] = append(w.src.items[folder], model.Meta{
		EntryID: entry, FolderPath: folder, InternetMessageID: msgID, Subject: "subject " + entry,
		SizeBytes: size, ReceivedAt: received, ModifiedAt: received, Importance: 1,
	})
	w.src.content[entry] = model.Content{Body: body}
}

func (w *world) remove(folder, entry string) {
	var keep []model.Meta
	for _, m := range w.src.items[folder] {
		if m.EntryID != entry {
			keep = append(keep, m)
		}
	}
	w.src.items[folder] = keep
}

func (w *world) edit(folder, entry string, fn func(*model.Meta)) {
	for i := range w.src.items[folder] {
		if w.src.items[folder][i].EntryID == entry {
			fn(&w.src.items[folder][i])
		}
	}
}

func opts() Options {
	return Options{MirrorDeletions: true, MassDeletePercent: 50, MassDeleteMin: 10, BodyMaxChars: 1000, BatchSize: 2}
}

func (w *world) run(o Options) (Result, error) {
	e := &Engine{Src: w.src, DB: w.db, Opt: o}
	return e.Run(context.Background())
}

func (w *world) mustRun(t *testing.T, o Options) Result {
	t.Helper()
	r, err := w.run(o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return r
}

func (w *world) row(entry string) *fakeRow { return w.db.rows[rowKey(mailbox, entry)] }

func standard() *world {
	w := newWorld()
	w.folder(`me\Inbox`)
	w.folder(`me\Sent`)
	w.add(`me\Inbox`, "e1", "<1>", 100, day(1), "one")
	w.add(`me\Sent`, "e3", "<3>", 300, day(3), "three")
	w.add(`me\Inbox`, "e2", "<2>", 200, day(2), "two")
	return w
}

// ---- tests ----

func TestFirstRunStoresEverythingNewestFirstAndTheSecondRunReadsNothing(t *testing.T) {
	w := standard()
	r := w.mustRun(t, opts())
	if r.Added != 3 || r.Updated != 0 || r.Failed != 0 || r.Deleted != 0 {
		t.Errorf("first run: %+v", r)
	}
	if got := strings.Join(w.src.fetched, ","); got != "e3,e2,e1" {
		t.Errorf("the newest mail is read first, got %s", got)
	}
	if len(w.db.rows) != 3 || w.row("e2").m.Body != "two" || w.row("e2").m.StoreKey != mailbox {
		t.Errorf("rows: %d", len(w.db.rows))
	}
	if len(w.db.runs) != 1 || w.db.runs[0].Added != 3 || w.db.runs[0].Kind != "sync" {
		t.Errorf("the run is recorded: %+v", w.db.runs)
	}

	w.src.fetched = nil
	r = w.mustRun(t, opts())
	if r.Added+r.Updated+r.Moved+r.Deleted != 0 || r.Unchanged != 3 || len(w.src.fetched) != 0 {
		t.Errorf("an unchanged mailbox changes nothing and reads nothing: %+v fetched=%v", r, w.src.fetched)
	}
}

func TestReadingAMailDoesNotMakeItBeReadAgain(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.src.fetched = nil
	// Outlook moves the modification time and may change the importance when a mail is read or flagged
	w.edit(`me\Inbox`, "e1", func(m *model.Meta) { m.ModifiedAt = day(10); m.Importance = 2 })
	r := w.mustRun(t, opts())
	if r.Updated != 1 || r.Added != 0 || len(w.src.fetched) != 0 {
		t.Errorf("a metadata change is an update without a read: %+v fetched=%v", r, w.src.fetched)
	}
	if got := w.row("e1").m; !got.ModifiedAt.Equal(day(10)) || got.Importance != 2 || got.Body != "one" {
		t.Errorf("row after update: %+v", got)
	}
}

func TestAChangedSizeIsReadAgain(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.src.fetched = nil
	w.edit(`me\Inbox`, "e1", func(m *model.Meta) { m.SizeBytes = 150 })
	w.src.content["e1"] = model.Content{Body: "one, edited"}
	r := w.mustRun(t, opts())
	if r.Updated != 1 || r.Added != 0 || strings.Join(w.src.fetched, ",") != "e1" || w.row("e1").m.Body != "one, edited" {
		t.Errorf("%+v fetched=%v body=%q", r, w.src.fetched, w.row("e1").m.Body)
	}
}

func TestAMovedMailIsFollowedNotReadAgain(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	idBefore := w.row("e2").id
	w.src.fetched = nil
	// moving an item to another folder gives it a new entry id; the Message-ID and the size stay
	w.remove(`me\Inbox`, "e2")
	w.add(`me\Sent`, "e2-moved", "<2>", 200, day(2), "two")
	r := w.mustRun(t, opts())
	if r.Moved != 1 || r.Deleted != 0 || r.Added != 0 || len(w.src.fetched) != 0 {
		t.Errorf("a move is not a delete and a new mail: %+v fetched=%v", r, w.src.fetched)
	}
	if w.row("e2") != nil || w.row("e2-moved") == nil || w.row("e2-moved").id != idBefore || w.row("e2-moved").m.FolderPath != `me\Sent` {
		t.Errorf("the stored message follows the item")
	}
}

func TestDeletionsAreMirrored(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.remove(`me\Inbox`, "e1")
	r := w.mustRun(t, opts())
	if r.Deleted != 1 || len(w.db.rows) != 2 || w.row("e1") != nil {
		t.Errorf("%+v rows=%d", r, len(w.db.rows))
	}
}

func TestMirroringCanBeSwitchedOff(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.remove(`me\Inbox`, "e1")
	o := opts()
	o.MirrorDeletions = false
	r := w.mustRun(t, o)
	if r.Deleted != 0 || r.DeletionsBlocked != 1 || w.row("e1") == nil {
		t.Errorf("%+v", r)
	}
}

func TestAMassDeletionIsRefusedUnlessAllowed(t *testing.T) {
	w := newWorld()
	w.folder(`me\Inbox`)
	for i := 0; i < 20; i++ {
		w.add(`me\Inbox`, fmt.Sprintf("e%02d", i), fmt.Sprintf("<%d>", i), int64(100+i), day(1).Add(time.Duration(i)*time.Minute), "text")
	}
	w.mustRun(t, opts())

	// a clean up of one mail is fine
	w.remove(`me\Inbox`, "e00")
	if r := w.mustRun(t, opts()); r.Deleted != 1 {
		t.Errorf("a small deletion goes through: %+v", r)
	}

	// 14 of the remaining 19 vanish: a store went missing, not a clean up
	for i := 1; i < 15; i++ {
		w.remove(`me\Inbox`, fmt.Sprintf("e%02d", i))
	}
	r := w.mustRun(t, opts())
	if r.Deleted != 0 || r.DeletionsBlocked != 14 || len(w.db.rows) != 19 {
		t.Errorf("a mass deletion is refused: %+v rows=%d", r, len(w.db.rows))
	}
	o := opts()
	o.AllowMassDelete = true
	r = w.mustRun(t, o)
	if r.Deleted != 14 || len(w.db.rows) != 5 {
		t.Errorf("with the override it goes through: %+v rows=%d", r, len(w.db.rows))
	}
}

func TestAFolderThatCannotBeScannedBlocksAllDeletionsButNotTheRest(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.src.scanErr[`me\Sent`] = errors.New("the folder is damaged")
	w.remove(`me\Inbox`, "e1")
	w.add(`me\Inbox`, "e4", "<4>", 400, day(4), "four")
	r, err := w.run(opts())
	if err != nil {
		t.Fatalf("a damaged folder does not fail the run: %v", err)
	}
	if r.Deleted != 0 || r.ScanErrors != 1 || r.DeletionsBlocked != 2 {
		t.Errorf("nothing may be deleted when part of the mailbox could not be seen: %+v", r)
	}
	if w.row("e3") == nil || w.row("e1") == nil {
		t.Error("the mails of the damaged folder must survive")
	}
	if w.row("e4") == nil || r.Added != 1 {
		t.Error("new mail in the healthy folder is still stored")
	}
	if w.db.runs[len(w.db.runs)-1].Note == "" {
		t.Error("the run record says that the scan was incomplete")
	}
}

func TestAnIncompleteFolderListBlocksDeletions(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.src.foldersErr = fmt.Errorf("%w: a folder could not be read", source.ErrPartial)
	w.remove(`me\Inbox`, "e1")
	r := w.mustRun(t, opts())
	if r.Deleted != 0 || r.DeletionsBlocked != 1 || r.ScanErrors != 1 {
		t.Errorf("%+v", r)
	}
}

func TestAnUnreadableItemIsStoredWithoutTextAndNotReadAgainUnlessAsked(t *testing.T) {
	w := standard()
	w.src.fetchErr["e2"] = errors.New("the item is corrupt")
	r := w.mustRun(t, opts())
	if r.Added != 3 || r.Failed != 1 {
		t.Errorf("%+v", r)
	}
	if got := w.row("e2").m; got.FetchError == "" || got.Body != "" || got.Subject != "subject e2" {
		t.Errorf("stored with what the scan knows: %+v", got)
	}

	w.src.fetched = nil
	if r := w.mustRun(t, opts()); r.Failed != 0 || len(w.src.fetched) != 0 {
		t.Errorf("a failed item is not read again by accident: %+v fetched=%v", r, w.src.fetched)
	}

	delete(w.src.fetchErr, "e2")
	o := opts()
	o.RetryFailed = true
	r = w.mustRun(t, o)
	if r.Updated != 1 || strings.Join(w.src.fetched, ",") != "e2" || w.row("e2").m.FetchError != "" || w.row("e2").m.Body != "two" {
		t.Errorf("-retry-failed reads it again: %+v fetched=%v", r, w.src.fetched)
	}
}

func TestAnItemThatVanishedBetweenScanAndReadIsSkipped(t *testing.T) {
	w := standard()
	w.src.fetchErr["e1"] = fmt.Errorf("gone: %w", source.ErrGone)
	r := w.mustRun(t, opts())
	if r.Skipped != 1 || r.Failed != 0 || r.Added != 2 || w.row("e1") != nil {
		t.Errorf("%+v", r)
	}
}

func TestALimitMakesARunPartialAndTheNextRunContinues(t *testing.T) {
	w := standard()
	o := opts()
	o.Limit = 2
	r := w.mustRun(t, o)
	if r.Added != 2 || strings.Join(w.src.fetched, ",") != "e3,e2" || len(w.db.rows) != 2 {
		t.Errorf("only the newest two: %+v fetched=%v", r, w.src.fetched)
	}
	w.src.fetched = nil
	r = w.mustRun(t, opts())
	if r.Added != 1 || strings.Join(w.src.fetched, ",") != "e1" || len(w.db.rows) != 3 {
		t.Errorf("the next run carries on where the last stopped: %+v fetched=%v", r, w.src.fetched)
	}
}

func TestAFolderFilterLeavesTheOtherFoldersAlone(t *testing.T) {
	w := standard()
	w.mustRun(t, opts())
	w.src.fetched = nil
	w.remove(`me\Inbox`, "e1")                          // gone from a folder outside the filter
	w.add(`me\Inbox`, "e5", "<5>", 500, day(5), "five") // new in a folder outside the filter
	w.add(`me\Sent`, "e6", "<6>", 600, day(6), "six")   // new in the filtered folder
	o := opts()
	o.FolderFilter = "sent"
	r := w.mustRun(t, o)
	if r.Added != 1 || r.Deleted != 0 || w.row("e6") == nil || w.row("e5") != nil || w.row("e1") == nil {
		t.Errorf("only the filtered folder is looked at: %+v", r)
	}
}

func TestAStoreFilterSelectsStores(t *testing.T) {
	w := standard()
	w.src.stores = append(w.src.stores, source.Store{Key: `pst:d:\mail\old.pst`, Kind: "pst", DisplayName: "Old"})
	w.folderIn(`pst:d:\mail\old.pst`, `Old\Inbox`)
	w.add(`Old\Inbox`, "p1", "<p1>", 50, day(1), "old mail")
	o := opts()
	o.StoreFilter = "pst"
	r := w.mustRun(t, o)
	if r.Added != 1 || len(w.db.rows) != 1 {
		t.Errorf("only the pst: %+v rows=%d", r, len(w.db.rows))
	}
	if _, err := w.run(Options{StoreFilter: "nothing-matches", BatchSize: 1, BodyMaxChars: 100}); err == nil {
		t.Error("a filter that matches no store is an error, not a silent success")
	}
}

func TestADryRunChangesNothing(t *testing.T) {
	w := standard()
	o := opts()
	o.DryRun = true
	r := w.mustRun(t, o)
	if len(w.db.rows) != 0 || len(w.src.fetched) != 0 || len(w.db.runs) != 0 || r.Added != 0 {
		t.Errorf("dry run wrote or read: rows=%d fetched=%v runs=%d %+v", len(w.db.rows), w.src.fetched, len(w.db.runs), r)
	}
}

func TestALongBodyIsCutAtACharacterBoundary(t *testing.T) {
	w := newWorld()
	w.folder(`me\Inbox`)
	w.add(`me\Inbox`, "e1", "<1>", 100, day(1), "héllo wörld, ünïcode")
	o := opts()
	o.BodyMaxChars = 5
	w.mustRun(t, o)
	got := w.row("e1").m
	if got.Body != "héllo" || !got.BodyTruncated || !utf8.ValidString(got.Body) {
		t.Errorf("body %q truncated=%v", got.Body, got.BodyTruncated)
	}
}

func TestAnInterruptedRunKeepsWhatWasRead(t *testing.T) {
	w := newWorld()
	w.folder(`me\Inbox`)
	for i := 1; i <= 5; i++ {
		w.add(`me\Inbox`, fmt.Sprintf("e%d", i), fmt.Sprintf("<%d>", i), int64(i), day(i), "text")
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.src.cancel, w.src.cancelAfter = cancel, 2
	o := opts()
	o.BatchSize = 10 // the two messages are still in the batch when the run is interrupted
	e := &Engine{Src: w.src, DB: w.db, Opt: o}
	r, err := e.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("an interrupted run reports it: %v", err)
	}
	if len(w.db.rows) != 2 || r.Added != 2 {
		t.Errorf("what was read before the interruption is stored: rows=%d %+v", len(w.db.rows), r)
	}
	if len(w.db.runs) != 1 {
		t.Errorf("the run is recorded even though the context ended: %d", len(w.db.runs))
	}
	// and the next run finishes the job
	w.src.cancelAfter = 0
	w.src.fetched = nil
	if r := w.mustRun(t, opts()); r.Added != 3 || len(w.db.rows) != 5 {
		t.Errorf("resumed: %+v rows=%d", r, len(w.db.rows))
	}
}

func TestNoStoreIsAnError(t *testing.T) {
	w := newWorld()
	w.src.stores = nil
	if _, err := w.run(opts()); err == nil || !strings.Contains(err.Error(), "no store to index") {
		t.Errorf("got %v", err)
	}
}

// ---- the plan on its own ----

func TestPlanMovesAsManyItemsAsThereAreStoredCandidates(t *testing.T) {
	k := func(id int64) model.Known { return model.Known{ID: id, InternetMessageID: "<x>", SizeBytes: 10} }
	known := map[string]model.Known{"old5": k(5), "old6": k(6), "old7": {ID: 7, InternetMessageID: "", SizeBytes: 10}}
	cur := func(entry string, d int) model.Meta {
		return model.Meta{EntryID: entry, InternetMessageID: "<x>", SizeBytes: 10, ReceivedAt: day(d), FolderPath: "f"}
	}
	current := map[string]model.Meta{"n1": cur("n1", 3), "n2": cur("n2", 2), "n3": cur("n3", 1)}
	p := buildPlan(current, known, false)
	if len(p.move) != 2 || p.move[0].ID != 5 || p.move[1].ID != 6 {
		t.Errorf("two stored candidates, two moves: %+v", p.move)
	}
	if len(p.fetch) != 1 || p.fetch[0].EntryID != "n3" {
		t.Errorf("the third new item is read (the older one): %+v", p.fetch)
	}
	if len(p.remove) != 1 || p.remove[0] != 7 {
		t.Errorf("a stored message without a Message-ID cannot be matched and is gone: %v", p.remove)
	}
}

func TestMassDelete(t *testing.T) {
	cases := []struct {
		n, total, pct, min int
		want               bool
	}{
		{1, 20, 10, 0, false},    // 5% of the index: below the 10% threshold
		{15, 20, 50, 10, true},   // 75% and more than 10 messages
		{5, 20, 50, 10, false},   // under the minimum count
		{12, 100, 50, 10, false}, // under the percentage
		{0, 0, 10, 0, false},
		{2000, 100000, 10, 1000, false}, // a real clean up of 2%
		{20000, 100000, 10, 1000, true}, // 20%
	}
	for _, c := range cases {
		if got := massDelete(c.n, c.total, c.pct, c.min); got != c.want {
			t.Errorf("massDelete(%d of %d, %d%%, min %d) = %v, want %v", c.n, c.total, c.pct, c.min, got, c.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	if s, cut := truncateRunes("abc", 5); s != "abc" || cut {
		t.Error("short")
	}
	if s, cut := truncateRunes("abcdef", 3); s != "abc" || !cut {
		t.Error("ascii")
	}
	if s, cut := truncateRunes("日本語です", 3); s != "日本語" || !cut {
		t.Errorf("multi-byte: %q", s)
	}
	if s, cut := truncateRunes("日本語", 3); s != "日本語" || cut {
		t.Errorf("exactly max characters is not cut: %q %v", s, cut)
	}
	if s, cut := truncateRunes("abc", 0); s != "abc" || cut {
		t.Error("no limit")
	}
}
