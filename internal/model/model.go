// Package model holds the plain data types shared by the Outlook reader, the sync engine, the database and the web page.
package model

import "time"

// Meta is what the bulk scan of a folder returns for one item: everything except the body and the attachment names.
type Meta struct {
	EntryID           string
	FolderPath        string // "<store name>\Inbox\Reports": the Outlook folder path without the leading "\\"
	InternetMessageID string
	Subject           string
	SenderName        string
	SenderEmail       string
	ToNames           string // display names as Outlook shows them
	CcNames           string
	ReceivedAt        time.Time // UTC, zero when unknown
	ModifiedAt        time.Time // UTC, zero when unknown
	SizeBytes         int64
	Importance        int // 0 low, 1 normal, 2 high
	HasAttachments    bool
}

// Content is what reading a single item adds to its Meta.
type Content struct {
	Body            string
	AttachmentNames string // file names joined with "; "
}

// Message is one row of the index.
type Message struct {
	ID       int64 // database id, 0 until the message is stored
	StoreKey string
	Meta
	Content
	BodyTruncated bool
	FetchError    string // not empty when the item could not be read; the body is then empty
}

// Known is what the database holds about a stored message, for the comparison with the mailbox.
type Known struct {
	ID                int64
	FolderPath        string
	InternetMessageID string
	SizeBytes         int64
	ModifiedAt        time.Time
	FetchFailed       bool
}

// MetaUpdate changes the cheap fields of a stored message that Outlook changes without touching the text
// (read state, flags and categories move the modification time).
type MetaUpdate struct {
	ID         int64
	FolderPath string
	ModifiedAt time.Time
	Importance int
}

// MoveUpdate re-points a stored message at the new Outlook item after the item was moved to another folder.
type MoveUpdate struct {
	ID         int64
	EntryID    string
	FolderPath string
	ModifiedAt time.Time
}

// RunRecord is the summary of one sync run, kept in the database for the status page.
type RunRecord struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Kind       string
	Added      int
	Updated    int
	Moved      int
	Deleted    int
	Failed     int
	Note       string
}

// SearchParams is one search of the index.
type SearchParams struct {
	Query       string    // web search syntax: words, "exact phrase", OR, -excluded
	From        string    // part of the sender name or address
	Since       time.Time // received at or after, zero for no limit
	Until       time.Time // received before, zero for no limit
	Folder      string    // folder path; the folder and everything below it
	Attachments bool      // only messages with attachments
	Sort        string    // "relevance" (default when there is a query) or "date"
	Limit       int
	Offset      int
}

// Hit is one search result.
type Hit struct {
	ID             int64
	Subject        string
	SenderName     string
	SenderEmail    string
	ReceivedAt     time.Time
	FolderPath     string
	HasAttachments bool
	SizeBytes      int64
	EntryID        string
	Snippet        string // plain text; matches are wrapped in the control characters MarkStart and MarkEnd
	Rank           float64
}

// The characters that mark a match inside Hit.Snippet. They cannot occur in mail text that went through the sync.
const (
	MarkStart = "\x01"
	MarkEnd   = "\x02"
)

// SearchResult is one page of results.
type SearchResult struct {
	Hits      []Hit
	HasMore   bool
	Truncated bool // relevance was computed over the newest matches only, because there are more than the limit
	Took      time.Duration
}

// Status describes the index.
type Status struct {
	Messages      int64
	Oldest        time.Time
	Newest        time.Time
	DBSizeBytes   int64
	LastRun       *RunRecord
	ServerVersion string
}

// FolderCount is one folder of the index with the number of messages in it.
type FolderCount struct {
	Path  string
	Count int64
}
