// Package store defines what the sync engine and the web page need from the database. The PostgreSQL implementation
// is in internal/store/pg.
package store

import (
	"context"
	"errors"

	"maillens/internal/model"
)

var (
	// ErrLocked means another sync run holds the lock.
	ErrLocked = errors.New("another sync is already running")
	// ErrNotFound means the message does not exist.
	ErrNotFound = errors.New("message not found")
)

// Writer is what a sync run needs.
type Writer interface {
	// Lock takes the lock that keeps two sync runs apart. The returned function releases it.
	Lock(ctx context.Context) (release func(), err error)
	// Known returns the stored messages of a store, keyed by the Outlook entry id.
	Known(ctx context.Context, storeKey string) (map[string]model.Known, error)
	// Upsert stores messages, replacing the ones with the same store and entry id.
	Upsert(ctx context.Context, msgs []model.Message) error
	UpdateMeta(ctx context.Context, ups []model.MetaUpdate) error
	Move(ctx context.Context, mvs []model.MoveUpdate) error
	Delete(ctx context.Context, ids []int64) error
	RecordRun(ctx context.Context, r model.RunRecord) error
}

// Reader is what the web page and the command line search need.
type Reader interface {
	Search(ctx context.Context, p model.SearchParams) (model.SearchResult, error)
	Message(ctx context.Context, id int64) (model.Message, error)
	Status(ctx context.Context) (model.Status, error)
	Folders(ctx context.Context) ([]model.FolderCount, error)
}
