package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maillens/internal/model"
	"maillens/internal/store"
)

type fakeReader struct {
	lastSearch  model.SearchParams
	searchRes   model.SearchResult
	searchErr   error
	msg         model.Message
	msgErr      error
	status      model.Status
	statusCalls int
	folders     []model.FolderCount
	folderCalls int
}

func (f *fakeReader) Search(_ context.Context, p model.SearchParams) (model.SearchResult, error) {
	f.lastSearch = p
	return f.searchRes, f.searchErr
}
func (f *fakeReader) Message(context.Context, int64) (model.Message, error) { return f.msg, f.msgErr }
func (f *fakeReader) Status(context.Context) (model.Status, error) {
	f.statusCalls++
	return f.status, nil
}
func (f *fakeReader) Folders(context.Context) ([]model.FolderCount, error) {
	f.folderCalls++
	return f.folders, nil
}

var _ store.Reader = (*fakeReader)(nil)

const local = "127.0.0.1:8765"

func get(t *testing.T, h http.Handler, host, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newServer(r *fakeReader) http.Handler { return New(r, nil, 25).Handler() }

func TestSearchPassesTheParametersOn(t *testing.T) {
	r := &fakeReader{}
	h := newServer(r)
	rec := get(t, h, local, "/api/search?q=invoice+-lunch&from=jane&since=2026-01-15&until=2026-01-20&folder=box%5CInbox&attach=1&sort=date&page=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	p := r.lastSearch
	if p.Query != "invoice -lunch" || p.From != "jane" || p.Folder != `box\Inbox` || !p.Attachments || p.Sort != "date" {
		t.Errorf("params: %+v", p)
	}
	if want := time.Date(2026, 1, 15, 0, 0, 0, 0, time.Local); !p.Since.Equal(want) {
		t.Errorf("since %v, want %v", p.Since, want)
	}
	if want := time.Date(2026, 1, 21, 0, 0, 0, 0, time.Local); !p.Until.Equal(want) {
		t.Errorf("until %v: the end day itself is included, so the limit is the start of the next day (%v)", p.Until, want)
	}
	if p.Limit != 25 || p.Offset != 50 {
		t.Errorf("page 3 of 25: limit %d offset %d", p.Limit, p.Offset)
	}

	get(t, h, local, "/api/search")
	if r.lastSearch.Sort != "relevance" || r.lastSearch.Offset != 0 || !r.lastSearch.Since.IsZero() {
		t.Errorf("defaults: %+v", r.lastSearch)
	}
}

func TestSearchResponseEscapesTheSnippet(t *testing.T) {
	r := &fakeReader{searchRes: model.SearchResult{
		Hits: []model.Hit{{
			ID: 7, Subject: "Invoice", SenderName: "Jane", SenderEmail: "jane@example.com",
			ReceivedAt: time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC), FolderPath: `box\Inbox`, HasAttachments: true, SizeBytes: 1234, EntryID: "ENTRY",
			Snippet: "see <script>alert(1)</script>\n  " + model.MarkStart + "invoice" + model.MarkEnd + " & \"more\"",
		}},
		HasMore: true, Truncated: true, Took: 42 * time.Millisecond,
	}}
	rec := get(t, newServer(r), local, "/api/search?q=invoice")
	var out searchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) != 1 || !out.HasMore || !out.Truncated || out.TookMS != 42 || out.Page != 1 {
		t.Fatalf("response: %+v", out)
	}
	h := out.Hits[0]
	if h.ID != 7 || h.ReceivedAt != "2026-11-20T09:30:00Z" || h.Folder != `box\Inbox` || !h.HasAttachments {
		t.Errorf("hit: %+v", h)
	}
	if strings.Contains(h.SnippetHTML, "<script>") || !strings.Contains(h.SnippetHTML, "&lt;script&gt;") {
		t.Errorf("mail text must never become markup: %s", h.SnippetHTML)
	}
	if !strings.Contains(h.SnippetHTML, "<mark>invoice</mark>") || !strings.Contains(h.SnippetHTML, "&amp;") || strings.Contains(h.SnippetHTML, "\n") {
		t.Errorf("marks, entities and whitespace: %s", h.SnippetHTML)
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	h := newServer(&fakeReader{})
	bad := []string{
		"/api/search?since=2026-13-45",
		"/api/search?until=yesterday",
		"/api/search?page=0",
		"/api/search?page=abc",
		"/api/search?page=1001",
		"/api/search?q=" + strings.Repeat("a", maxQueryChars+1),
		"/api/search?from=" + strings.Repeat("b", maxFieldChars+1),
	}
	for _, target := range bad {
		if rec := get(t, h, local, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%.60s: status %d, want 400", target, rec.Code)
		}
	}
}

func TestOnlyLoopbackHostsAreAnswered(t *testing.T) {
	h := newServer(&fakeReader{})
	for _, host := range []string{"evil.example", "evil.example:8765", "127.0.0.1.evil.example", "192.168.1.5:8765", "10.0.0.1"} {
		if rec := get(t, h, host, "/api/status"); rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, rec.Code)
		}
	}
	for _, host := range []string{"127.0.0.1:8765", "localhost:8765", "localhost", "[::1]:8765", "127.0.0.2:80"} {
		if rec := get(t, h, host, "/api/status"); rec.Code != http.StatusOK {
			t.Errorf("Host %q: status %d, want 200", host, rec.Code)
		}
	}
}

func TestErrorsDoNotLeakInternals(t *testing.T) {
	r := &fakeReader{searchErr: errors.New(`pq: password authentication failed for user "mailuser42" at host db1.internal`)}
	rec := get(t, newServer(r), local, "/api/search?q=x")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	for _, leak := range []string{"password", "db1", "mailuser42", "pq:"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the response leaks %q: %s", leak, rec.Body)
		}
	}
	if !strings.Contains(rec.Body.String(), "failed") {
		t.Errorf("the browser is still told that it failed: %s", rec.Body)
	}
}

func TestMessageEndpoint(t *testing.T) {
	r := &fakeReader{msg: model.Message{
		ID: 9, Meta: model.Meta{EntryID: "ENTRY", Subject: "Hello", SenderName: "Jane", SenderEmail: "jane@example.com", ToNames: "Me", ReceivedAt: time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC), FolderPath: `box\Inbox`, HasAttachments: true, SizeBytes: 55},
		Content: model.Content{Body: "Hi there", AttachmentNames: "a.pdf"},
	}}
	h := newServer(r)
	rec := get(t, h, local, "/api/message/9")
	var m messageDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	if m.Subject != "Hello" || m.Body != "Hi there" || m.Attachments != "a.pdf" || m.OutlookURL != "outlook:ENTRY" || m.BodyCut {
		t.Errorf("message: %+v", m)
	}

	for _, id := range []string{"abc", "0", "-5", "1.5"} {
		if rec := get(t, h, local, "/api/message/"+id); rec.Code != http.StatusBadRequest {
			t.Errorf("id %q: status %d, want 400", id, rec.Code)
		}
	}

	r.msgErr = store.ErrNotFound
	if rec := get(t, h, local, "/api/message/9"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}

	r.msgErr = nil
	r.msg.Body = strings.Repeat("é", viewMaxBytes) // 2 bytes each: twice the limit
	rec = get(t, h, local, "/api/message/9")
	var big messageDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &big)
	if !big.BodyCut || len(big.Body) > viewMaxBytes || strings.ContainsRune(big.Body, '�') {
		t.Errorf("a huge body is cut for the view, on a character boundary: %d bytes, cut=%v", len(big.Body), big.BodyCut)
	}
}

func TestStatusAndFoldersAreCached(t *testing.T) {
	r := &fakeReader{
		status:  model.Status{Messages: 5, DBSizeBytes: 1000, LastRun: &model.RunRecord{Added: 3, Deleted: 1, FinishedAt: time.Date(2026, 11, 22, 8, 0, 0, 0, time.UTC)}},
		folders: []model.FolderCount{{Path: `box\Inbox`, Count: 5}},
	}
	h := newServer(r)
	for i := 0; i < 3; i++ {
		get(t, h, local, "/api/status")
		get(t, h, local, "/api/folders")
	}
	if r.statusCalls != 1 || r.folderCalls != 1 {
		t.Errorf("the database is asked once: status %d, folders %d", r.statusCalls, r.folderCalls)
	}
	var st statusDTO
	_ = json.Unmarshal(get(t, h, local, "/api/status").Body.Bytes(), &st)
	if st.Messages != 5 || st.LastRun == nil || st.LastRun.Added != 3 || st.LastRun.FinishedAt != "2026-11-22T08:00:00Z" {
		t.Errorf("status: %+v", st)
	}
	var fs []folderDTO
	_ = json.Unmarshal(get(t, h, local, "/api/folders").Body.Bytes(), &fs)
	if len(fs) != 1 || fs[0].Path != `box\Inbox` || fs[0].Count != 5 {
		t.Errorf("folders: %+v", fs)
	}
}

func TestPageStaticFilesAndSecurityHeaders(t *testing.T) {
	h := newServer(&fakeReader{})

	rec := get(t, h, local, "/")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "MailLens") {
		t.Errorf("index: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	for name, want := range map[string]string{
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "default-src 'self'",
	} {
		if got := rec.Header().Get(name); !strings.Contains(got, want) {
			t.Errorf("header %s = %q, want it to contain %q", name, got, want)
		}
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no CORS header: other sites must not be able to read the answers")
	}
	// the page may not rely on inline script or style, because the policy forbids them
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, " style=") || strings.Contains(body, " onclick=") {
		t.Error("the page uses inline script or style")
	}

	if rec := get(t, h, local, "/static/app.js"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "use strict") {
		t.Errorf("app.js: %d", rec.Code)
	}
	if rec := get(t, h, local, "/static/style.css"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "--accent") {
		t.Errorf("style.css: %d", rec.Code)
	}
	if rec := get(t, h, local, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: %d", rec.Code)
	}
	post := httptest.NewRequest(http.MethodPost, "/api/search", nil)
	post.Host = local
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, post)
	if prec.Code != http.StatusMethodNotAllowed {
		t.Errorf("the interface only reads: POST gave %d", prec.Code)
	}
}

func TestSnippetHTML(t *testing.T) {
	cases := map[string]string{
		"plain text":     "plain text",
		"a   b\n\tc":     "a b c",
		`<b>"x"</b> & y`: "&lt;b&gt;&#34;x&#34;&lt;/b&gt; &amp; y",
		"x " + model.MarkStart + "hit" + model.MarkEnd + " y": "x <mark>hit</mark> y",
		"": "",
	}
	for in, want := range cases {
		if got := SnippetHTML(in); got != want {
			t.Errorf("SnippetHTML(%q) = %q, want %q", in, got, want)
		}
	}
}
