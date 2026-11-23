// Package web is the search page: a small HTTP server with a JSON interface and a single page that uses it. The
// index holds the text of all mail and the page has no sign-in, so the server only answers requests that were
// addressed to the loopback interface (a web page of another site cannot reach it through DNS tricks), only reads,
// and never sends CORS headers.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"maillens/internal/model"
	"maillens/internal/store"
)

//go:embed static
var staticFS embed.FS

const (
	maxQueryChars = 500
	maxFieldChars = 300
	maxPage       = 1000
	viewMaxBytes  = 1 << 20 // the message view shows at most this much of a body
)

// Server answers the search page and its JSON interface.
type Server struct {
	reader   store.Reader
	log      *slog.Logger
	pageSize int
	index    []byte
	mux      *http.ServeMux

	mu      sync.Mutex
	status  cacheEntry[model.Status]
	folders cacheEntry[[]model.FolderCount]
}

type cacheEntry[T any] struct {
	at time.Time
	v  T
	ok bool
}

// New builds the server. pageSize is the number of results per page.
func New(r store.Reader, log *slog.Logger, pageSize int) *Server {
	s := &Server{reader: r, log: log, pageSize: pageSize}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the folder is embedded at build time
	}
	if s.index, err = fs.ReadFile(sub, "index.html"); err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))
	mux.HandleFunc("GET /{$}", s.serveIndex)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/message/{id}", s.message)
	mux.HandleFunc("GET /api/status", s.statusHandler)
	mux.HandleFunc("GET /api/folders", s.foldersHandler)
	s.mux = mux
	return s
}

// Handler is the server behind the guard that checks the Host header and sets the security headers.
func (s *Server) Handler() http.Handler { return s.guard(s.mux) }

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// allowedHost accepts only requests addressed to a loopback name or address: a page of another site that makes the
// browser resolve its own name to 127.0.0.1 still sends its own name in the Host header.
func allowedHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) serveIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.index)
}

// ---- JSON ----

type hitDTO struct {
	ID             int64  `json:"id"`
	Subject        string `json:"subject"`
	SenderName     string `json:"sender_name"`
	SenderEmail    string `json:"sender_email"`
	ReceivedAt     string `json:"received_at"`
	Folder         string `json:"folder"`
	HasAttachments bool   `json:"has_attachments"`
	SizeBytes      int64  `json:"size_bytes"`
	SnippetHTML    string `json:"snippet_html"`
}

type searchResponse struct {
	Hits      []hitDTO `json:"hits"`
	Page      int      `json:"page"`
	HasMore   bool     `json:"has_more"`
	Truncated bool     `json:"truncated"`
	TookMS    int64    `json:"took_ms"`
}

type messageDTO struct {
	ID             int64  `json:"id"`
	Subject        string `json:"subject"`
	SenderName     string `json:"sender_name"`
	SenderEmail    string `json:"sender_email"`
	To             string `json:"to"`
	Cc             string `json:"cc"`
	ReceivedAt     string `json:"received_at"`
	Folder         string `json:"folder"`
	HasAttachments bool   `json:"has_attachments"`
	Attachments    string `json:"attachments"`
	SizeBytes      int64  `json:"size_bytes"`
	Body           string `json:"body"`
	BodyCut        bool   `json:"body_cut"` // the stored body was cut at sync time, or is cut for this view
	FetchError     string `json:"fetch_error"`
	OutlookURL     string `json:"outlook_url"`
}

type statusDTO struct {
	Messages      int64   `json:"messages"`
	Oldest        string  `json:"oldest"`
	Newest        string  `json:"newest"`
	DBSizeBytes   int64   `json:"db_size_bytes"`
	ServerVersion string  `json:"server_version"`
	LastRun       *runDTO `json:"last_run"`
}

type runDTO struct {
	FinishedAt string `json:"finished_at"`
	Added      int    `json:"added"`
	Updated    int    `json:"updated"`
	Moved      int    `json:"moved"`
	Deleted    int    `json:"deleted"`
	Failed     int    `json:"failed"`
	Note       string `json:"note"`
}

type folderDTO struct {
	Path  string `json:"path"`
	Count int64  `json:"count"`
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// SnippetHTML turns the snippet of a hit into HTML: everything is escaped, only the match markers become <mark>.
func SnippetHTML(raw string) string {
	raw = strings.Join(strings.Fields(raw), " ")
	esc := html.EscapeString(raw)
	esc = strings.ReplaceAll(esc, model.MarkStart, "<mark>")
	return strings.ReplaceAll(esc, model.MarkEnd, "</mark>")
}

func outlookURL(entryID string) string {
	if entryID == "" {
		return ""
	}
	return "outlook:" + entryID
}

// ---- handlers ----

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := model.SearchParams{
		Query:       strings.TrimSpace(q.Get("q")),
		From:        strings.TrimSpace(q.Get("from")),
		Folder:      strings.TrimSpace(q.Get("folder")),
		Attachments: q.Get("attach") == "1",
		Sort:        "relevance",
	}
	if q.Get("sort") == "date" {
		p.Sort = "date"
	}
	if utf8.RuneCountInString(p.Query) > maxQueryChars {
		writeError(w, http.StatusBadRequest, "the query is too long")
		return
	}
	if utf8.RuneCountInString(p.From) > maxFieldChars || utf8.RuneCountInString(p.Folder) > maxFieldChars {
		writeError(w, http.StatusBadRequest, "a filter is too long")
		return
	}
	var err error
	if p.Since, err = parseDay(q.Get("since"), 0); err != nil {
		writeError(w, http.StatusBadRequest, "since must be a date: YYYY-MM-DD")
		return
	}
	if p.Until, err = parseDay(q.Get("until"), 1); err != nil { // the day itself is included
		writeError(w, http.StatusBadRequest, "until must be a date: YYYY-MM-DD")
		return
	}
	page := 1
	if v := q.Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 || page > maxPage {
			writeError(w, http.StatusBadRequest, "page is out of range")
			return
		}
	}
	p.Limit = s.pageSize
	p.Offset = (page - 1) * s.pageSize

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.reader.Search(ctx, p)
	if err != nil {
		s.fail(w, r, "search", err)
		return
	}
	out := searchResponse{Hits: make([]hitDTO, 0, len(res.Hits)), Page: page, HasMore: res.HasMore, Truncated: res.Truncated, TookMS: res.Took.Milliseconds()}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, hitDTO{
			ID: h.ID, Subject: h.Subject, SenderName: h.SenderName, SenderEmail: h.SenderEmail,
			ReceivedAt: rfc3339(h.ReceivedAt), Folder: h.FolderPath, HasAttachments: h.HasAttachments,
			SizeBytes: h.SizeBytes, SnippetHTML: SnippetHTML(h.Snippet),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// parseDay reads a date typed as YYYY-MM-DD in the local time zone, plus addDays; empty is no limit.
func parseDay(s string, addDays int) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return t.AddDate(0, 0, addDays), nil
}

func (s *Server) message(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "bad message id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	m, err := s.reader.Message(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	}
	if err != nil {
		s.fail(w, r, "message", err)
		return
	}
	body, cut := m.Body, m.BodyTruncated
	if len(body) > viewMaxBytes {
		body = strings.ToValidUTF8(body[:viewMaxBytes], "")
		cut = true
	}
	writeJSON(w, http.StatusOK, messageDTO{
		ID: m.ID, Subject: m.Subject, SenderName: m.SenderName, SenderEmail: m.SenderEmail,
		To: m.ToNames, Cc: m.CcNames, ReceivedAt: rfc3339(m.ReceivedAt), Folder: m.FolderPath,
		HasAttachments: m.HasAttachments, Attachments: m.AttachmentNames, SizeBytes: m.SizeBytes,
		Body: body, BodyCut: cut, FetchError: m.FetchError, OutlookURL: outlookURL(m.EntryID),
	})
}

func (s *Server) statusHandler(w http.ResponseWriter, r *http.Request) {
	st, err := cachedValue(s, &s.status, 30*time.Second, func() (model.Status, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		return s.reader.Status(ctx)
	})
	if err != nil {
		s.fail(w, r, "status", err)
		return
	}
	out := statusDTO{Messages: st.Messages, Oldest: rfc3339(st.Oldest), Newest: rfc3339(st.Newest), DBSizeBytes: st.DBSizeBytes, ServerVersion: st.ServerVersion}
	if lr := st.LastRun; lr != nil {
		out.LastRun = &runDTO{FinishedAt: rfc3339(lr.FinishedAt), Added: lr.Added, Updated: lr.Updated, Moved: lr.Moved, Deleted: lr.Deleted, Failed: lr.Failed, Note: lr.Note}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) foldersHandler(w http.ResponseWriter, r *http.Request) {
	fs, err := cachedValue(s, &s.folders, 5*time.Minute, func() ([]model.FolderCount, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		return s.reader.Folders(ctx)
	})
	if err != nil {
		s.fail(w, r, "folders", err)
		return
	}
	out := make([]folderDTO, 0, len(fs))
	for _, f := range fs {
		out = append(out, folderDTO{Path: f.Path, Count: f.Count})
	}
	writeJSON(w, http.StatusOK, out)
}

// cachedValue returns the cached value of an entry that is younger than ttl, or loads it.
func cachedValue[T any](s *Server, e *cacheEntry[T], ttl time.Duration, load func() (T, error)) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ok && time.Since(e.at) < ttl {
		return e.v, nil
	}
	v, err := load()
	if err != nil {
		var zero T
		return zero, err
	}
	*e = cacheEntry[T]{at: time.Now(), v: v, ok: true}
	return v, nil
}

// fail logs the real error and tells the browser only that something went wrong: database messages can name tables,
// users and hosts.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return // the browser went away
	}
	if s.log != nil {
		s.log.Error("web request failed", "what", what, "error", err)
	}
	writeError(w, http.StatusInternalServerError, what+" failed; see the log of maillens")
}

// Serve runs the server until ctx ends.
func Serve(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr: addr, Handler: h,
		ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("the search page is open", "url", "http://"+addr+"/")
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return nil
	}
}
