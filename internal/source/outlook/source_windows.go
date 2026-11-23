package outlook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"sync"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows"

	"maillens/internal/model"
	"maillens/internal/source"
)

// Options selects what is read from Outlook.
type Options struct {
	Stores         []string // "mailbox" and/or "pst"
	ExcludeFolders []string
	StartIfNeeded  bool // start Outlook when it is not running
}

type storeRef struct {
	store   source.Store
	storeID string
	obj     *obj // kept open for the life of the Source
}

// Source reads the running Outlook. Outlook's object model is single threaded: every COM call runs on one goroutine
// that is locked to one operating system thread, and the methods hand their work over to it.
type Source struct {
	opt     Options
	log     *slog.Logger
	jobs    chan job
	stopped chan struct{}
	once    sync.Once
	version string // set before Open returns, read only afterwards

	// owned by the COM goroutine
	app, ns *obj
	stores  map[string]*storeRef
}

type job struct {
	fn   func()
	done chan struct{}
}

var _ source.Source = (*Source)(nil)

// Open connects to the running Outlook (or starts it when opt.StartIfNeeded is set). The caller must run at the same
// privilege level as Outlook, or COM refuses the connection.
func Open(opt Options, log *slog.Logger) (*Source, error) {
	s := &Source{
		opt: opt, log: log,
		jobs: make(chan job), stopped: make(chan struct{}),
		stores: map[string]*storeRef{},
	}
	ready := make(chan error, 1)
	go s.loop(ready)
	if err := <-ready; err != nil {
		<-s.stopped
		return nil, err
	}
	return s, nil
}

// Version is the Outlook version, for example "16.0.19127.20154".
func (s *Source) Version() string { return s.version }

func (s *Source) loop(ready chan<- error) {
	defer close(s.stopped)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		var oe *ole.OleError
		if !(errors.As(err, &oe) && oe.Code() == 1) { // 1 is S_FALSE: this thread was already initialised
			ready <- fmt.Errorf("COM initialisation failed: %w", err)
			return
		}
	}
	defer ole.CoUninitialize()

	if err := s.connect(); err != nil {
		ready <- err
		return
	}
	ready <- nil
	for j := range s.jobs {
		j.fn()
		close(j.done)
	}
	s.releaseAll()
}

func (s *Source) connect() error {
	unk, err := oleutil.GetActiveObject("Outlook.Application")
	if err != nil {
		if !s.opt.StartIfNeeded {
			return explainConnect(err)
		}
		if unk, err = oleutil.CreateObject("Outlook.Application"); err != nil {
			return explainConnect(err)
		}
	}
	disp, err := unk.QueryInterface(ole.IID_IDispatch)
	unk.Release()
	if err != nil {
		return fmt.Errorf("outlook: %w", err)
	}
	s.app = &obj{d: disp}
	if v, err := s.app.str("Version"); err == nil {
		s.version = v
	}
	ns, err := s.app.child("Session")
	if err != nil {
		s.app.release()
		s.app = nil
		return explainConnect(err)
	}
	s.ns = ns
	return nil
}

// explainConnect adds to a failed connection what people need to fix it. The same error comes when Outlook is not
// running and when it runs at another privilege level than this program.
func explainConnect(err error) error {
	code, scode := hresults(err)
	detail := ""
	if code != 0 {
		detail = fmt.Sprintf(" (HRESULT 0x%08X", code)
		if scode != 0 {
			detail += fmt.Sprintf(", 0x%08X", scode)
		}
		detail += ")"
	}
	level := "not elevated"
	if windows.GetCurrentProcessToken().IsElevated() {
		level = "elevated (administrator)"
	}
	return fmt.Errorf("cannot connect to Outlook%s: %w. Outlook must be running, and maillens must run at the same privilege level as Outlook: start it from a normal, non-administrator terminal. This process is %s", detail, err, level)
}

func (s *Source) releaseAll() {
	for _, r := range s.stores {
		r.obj.release()
	}
	s.stores = map[string]*storeRef{}
	s.ns.release()
	s.app.release()
}

// do runs fn on the COM goroutine and waits for it. A COM call that is running cannot be interrupted, so ctx is only
// looked at before the call is handed over.
func (s *Source) do(ctx context.Context, fn func() error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	j := job{done: make(chan struct{})}
	j.fn = func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("outlook: internal error: %v", r)
			}
		}()
		err = fn()
	}
	select {
	case s.jobs <- j:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-j.done
	return err
}

// Close releases every COM object and the connection. The Source must not be used afterwards.
func (s *Source) Close() error {
	s.once.Do(func() { close(s.jobs) })
	<-s.stopped
	return nil
}

// ---- stores ----

// Stores lists the primary mailbox and the data files that the configuration asks for.
func (s *Source) Stores(ctx context.Context) ([]source.Store, error) {
	var out []source.Store
	err := s.do(ctx, func() error {
		for _, r := range s.stores {
			r.obj.release()
		}
		s.stores = map[string]*storeRef{}
		coll, err := s.ns.child("Stores")
		if err != nil {
			return err
		}
		defer coll.release()
		n, err := coll.count()
		if err != nil {
			return err
		}
		for i := 1; i <= n; i++ {
			st, err := coll.at(i)
			if err != nil {
				s.log.Warn("a store could not be opened", "index", i, "error", err)
				continue
			}
			ref, ok := s.describe(st)
			if !ok {
				st.release()
				continue
			}
			if _, dup := s.stores[ref.store.Key]; dup {
				s.log.Warn("two stores have the same key, the second one is ignored", "store", ref.store.Key)
				st.release()
				continue
			}
			s.stores[ref.store.Key] = ref
			out = append(out, ref.store)
		}
		return nil
	})
	return out, err
}

// describe decides whether a store is indexed. It keeps the store object open when it is.
func (s *Source) describe(st *obj) (*storeRef, bool) {
	name, _ := st.str("DisplayName")
	typ, err := st.integer("ExchangeStoreType")
	if err != nil {
		s.log.Debug("store skipped: its type is unknown", "error", err)
		return nil, false
	}
	path, _ := st.str("FilePath") // fails for a store that is not a file
	kind, ok := ClassifyStore(int(typ), path)
	if !ok {
		s.log.Debug("store skipped: not the primary mailbox and not a .pst file", "exchange_store_type", typ)
		return nil, false
	}
	if !slices.Contains(s.opt.Stores, kind) {
		return nil, false
	}
	id, err := st.str("StoreID")
	if err != nil || id == "" {
		s.log.Warn("store skipped: it has no StoreID", "error", err)
		return nil, false
	}
	return &storeRef{
		store:   source.Store{Key: StoreKey(kind, name, path), Kind: kind, DisplayName: name, FilePath: path},
		storeID: id,
		obj:     st,
	}, true
}

// ---- folders ----

// Folders lists the mail folders of a store, leaving out Deleted Items, Junk E-mail, Drafts, Outbox, system folders
// and the folders of the exclude_folders setting.
func (s *Source) Folders(ctx context.Context, st source.Store) ([]source.Folder, error) {
	var out []source.Folder
	var partial error
	err := s.do(ctx, func() error {
		ref, ok := s.stores[st.Key]
		if !ok {
			return fmt.Errorf("unknown store %q", st.Key)
		}
		root, err := ref.obj.callChild("GetRootFolder")
		if err != nil {
			return err
		}
		defer root.release()
		w := &walker{s: s, ctx: ctx, ref: ref, skip: s.specialFolders(ref.obj)}
		if err := w.walk(root); err != nil {
			return err
		}
		out, partial = w.out, w.firstErr
		return nil
	})
	if err != nil {
		return out, err
	}
	if partial != nil {
		return out, fmt.Errorf("%w: %v", source.ErrPartial, partial)
	}
	return out, nil
}

// specialFolders returns the entry ids of the folders that are never indexed. Store.GetDefaultFolder fails for a
// store that lacks one of them, which is fine.
func (s *Source) specialFolders(store *obj) map[string]bool {
	skip := map[string]bool{}
	for _, t := range []int{3 /* Deleted Items */, 4 /* Outbox */, 16 /* Drafts */, 23 /* Junk E-mail */} {
		f, err := store.callChild("GetDefaultFolder", t)
		if err != nil {
			continue
		}
		if id, err := f.str("EntryID"); err == nil && id != "" {
			skip[id] = true
		}
		f.release()
	}
	return skip
}

type walker struct {
	s        *Source
	ctx      context.Context
	ref      *storeRef
	skip     map[string]bool
	out      []source.Folder
	firstErr error // the first folder that could not be read: the list is then incomplete
}

func (w *walker) note(err error) {
	if w.firstErr == nil {
		w.firstErr = err
	}
	w.s.log.Warn("a folder could not be read, the list of folders is incomplete", "error", err)
}

func (w *walker) walk(parent *obj) error {
	subs, err := parent.child("Folders")
	if err != nil {
		return err
	}
	defer subs.release()
	n, err := subs.count()
	if err != nil {
		return err
	}
	for i := 1; i <= n; i++ {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		f, err := subs.at(i)
		if err != nil {
			w.note(fmt.Errorf("folder %d: %w", i, err))
			continue
		}
		err = w.visit(f)
		f.release()
		if err != nil {
			return err // only a cancelled context comes back
		}
	}
	return nil
}

func (w *walker) visit(f *obj) error {
	name, _ := f.str("Name")
	id, _ := f.str("EntryID")
	if w.skip[id] || ExcludedByName(name) {
		return nil
	}
	full, err := f.str("FolderPath")
	if err != nil || full == "" {
		w.note(fmt.Errorf("folder %q has no path: %v", name, err))
		return nil
	}
	rel := RelFolderPath(full)
	if ExcludedByConfig(rel, w.s.opt.ExcludeFolders) {
		return nil
	}
	typ, terr := f.integer("DefaultItemType")
	if terr == nil && typ == 0 { // olMailItem
		if id == "" {
			w.note(fmt.Errorf("folder %q has no entry id", rel))
		} else {
			w.out = append(w.out, source.Folder{StoreKey: w.ref.store.Key, Path: rel, EntryID: id})
		}
	}
	if terr == nil && typ != 0 {
		return nil // calendars, contacts and tasks have no mail folders below them
	}
	if err := w.walk(f); err != nil {
		if w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		w.note(fmt.Errorf("subfolders of %q: %w", rel, err))
	}
	return nil
}

// ---- items ----

// ListMeta scans a folder with Outlook's bulk table: one call per row and column instead of one object per item, which
// is what makes a folder of hundreds of thousands of items bearable. The table cannot return whole bodies.
func (s *Source) ListMeta(ctx context.Context, f source.Folder, fn func(model.Meta) error) error {
	return s.do(ctx, func() error {
		ref, ok := s.stores[f.StoreKey]
		if !ok {
			return fmt.Errorf("unknown store %q", f.StoreKey)
		}
		folder, err := s.ns.callChild("GetFolderFromID", f.EntryID, ref.storeID)
		if err != nil {
			return fmt.Errorf("open folder %q: %w", f.Path, err)
		}
		defer folder.release()
		tbl, err := folder.callChild("GetTable")
		if err != nil {
			return fmt.Errorf("table of %q: %w", f.Path, err)
		}
		defer tbl.release()
		cols, err := tbl.child("Columns")
		if err != nil {
			return err
		}
		defer cols.release()
		v, err := cols.call("RemoveAll")
		if err != nil {
			return fmt.Errorf("columns of %q: %w", f.Path, err)
		}
		_ = v.Clear()
		for _, c := range tableColumns {
			v, err := cols.call("Add", c)
			if err != nil {
				return fmt.Errorf("add column %q: %w", c, err)
			}
			_ = v.Clear()
		}

		vals := make([]interface{}, len(tableColumns))
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			eot, err := tbl.value("EndOfTable")
			if err != nil {
				return err
			}
			if asBool(eot) {
				return nil
			}
			row, err := tbl.callChild("GetNextRow")
			if err != nil {
				return fmt.Errorf("read row of %q: %w", f.Path, err)
			}
			for i := range vals {
				vals[i] = nil
				cell, err := row.call("Item", i+1)
				if err != nil {
					continue // a property the item does not have
				}
				vals[i] = cell.Value()
				_ = cell.Clear()
			}
			row.release()
			m, ok := MetaFromRow(vals, f.Path)
			if !ok {
				continue
			}
			if err := fn(m); err != nil {
				if errors.Is(err, source.ErrStop) {
					return nil
				}
				return err
			}
		}
	})
}

// Fetch reads the body and the attachment names of one item. Reading a body can ask Outlook's security prompt for
// permission when no up to date antivirus program is registered; "maillens check" tests for that.
func (s *Source) Fetch(ctx context.Context, storeKey, entryID string, hasAttachments bool) (model.Content, error) {
	var c model.Content
	err := s.do(ctx, func() error {
		ref, ok := s.stores[storeKey]
		if !ok {
			return fmt.Errorf("unknown store %q", storeKey)
		}
		item, err := s.ns.callChild("GetItemFromID", entryID, ref.storeID)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("%w: %v", source.ErrGone, err)
			}
			return err
		}
		defer item.release()
		body, err := item.str("Body")
		if err != nil {
			return err
		}
		c.Body = body
		if hasAttachments {
			names, aerr := attachmentNames(item)
			if aerr != nil {
				s.log.Debug("attachment names are not available", "error", aerr) // the body is still worth indexing
			}
			c.AttachmentNames = JoinAttachmentNames(names)
		}
		return nil
	})
	return c, err
}

func attachmentNames(item *obj) ([]string, error) {
	atts, err := item.child("Attachments")
	if err != nil {
		return nil, err
	}
	defer atts.release()
	n, err := atts.count()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		a, err := atts.at(i)
		if err != nil {
			continue
		}
		name, _ := a.str("FileName")
		a.release()
		names = append(names, name)
	}
	return names, nil
}
