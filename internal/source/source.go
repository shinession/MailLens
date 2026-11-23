// Package source defines where mail comes from. The only real implementation reads the local Outlook through COM
// (internal/source/outlook); the sync engine only knows this interface, so it can be tested with a fake.
package source

import (
	"context"
	"errors"

	"maillens/internal/model"
)

// Store is one Outlook data store that is indexed: the primary mailbox or a PST file attached to the profile.
type Store struct {
	Key         string // stable key kept in the database
	Kind        string // "mailbox" or "pst"
	DisplayName string
	FilePath    string // the .pst file, empty for the mailbox
}

// Folder is one mail folder of a store.
type Folder struct {
	StoreKey string
	Path     string // Outlook folder path without the leading "\\", unique across stores
	EntryID  string
}

// Source reads mail. All methods may be slow; they stop early when ctx ends, with the limits of the underlying API.
type Source interface {
	// Stores lists the stores to index.
	Stores(ctx context.Context) ([]Store, error)
	// Folders lists the mail folders of a store. A folder that could not be listed makes the error wrap ErrPartial; the
	// folders that could be listed are still returned.
	Folders(ctx context.Context, st Store) ([]Folder, error)
	// ListMeta scans a folder in bulk and calls fn for every mail item, without reading the bodies.
	ListMeta(ctx context.Context, f Folder, fn func(model.Meta) error) error
	// Fetch reads the body (and the attachment names when hasAttachments) of one item. It returns ErrGone when the item
	// no longer exists.
	Fetch(ctx context.Context, storeKey, entryID string, hasAttachments bool) (model.Content, error)
	// Close releases the connection.
	Close() error
}

var (
	// ErrStop is returned by a ListMeta callback to end the scan early without an error.
	ErrStop = errors.New("stop")
	// ErrPartial marks a result that is incomplete because a part of the source could not be read.
	ErrPartial = errors.New("incomplete result")
	// ErrGone means the item was deleted or moved between the scan and the read.
	ErrGone = errors.New("item no longer exists")
)
