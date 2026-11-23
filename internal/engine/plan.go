package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"maillens/internal/model"
)

// plan is what a run has to do for one store, worked out from the scan of the mailbox and the stored messages.
type plan struct {
	fetch     []model.Meta       // new items, and items whose size changed: their text is read, newest first
	update    []model.MetaUpdate // items that only changed their flags or folder: no need to read them again
	move      []model.MoveUpdate // items moved to another folder: the stored message follows
	remove    []int64            // stored messages whose item is gone from the mailbox
	unchanged int
}

func moveKey(messageID string, size int64) string {
	return fmt.Sprintf("%s|%d", messageID, size)
}

// buildPlan compares the mailbox (current, keyed by entry id) with the index (known, keyed by entry id).
//
//   - an item the index lacks is new;
//   - an item whose size changed has new text; so has an item that could not be read the last time when retryFailed is set;
//   - an item that only changed its modification time or folder (reading a mail, flagging it or categorising it
//     does this) is updated without reading it;
//   - an item the index has but the mailbox lacks is gone, unless a new item has the same Message-ID and size:
//     then the item was moved to another folder (which gives it a new entry id), and the stored message follows it.
func buildPlan(current map[string]model.Meta, known map[string]model.Known, retryFailed bool) plan {
	var p plan
	var added []model.Meta
	for id, m := range current {
		k, ok := known[id]
		switch {
		case !ok:
			added = append(added, m)
		case k.SizeBytes != m.SizeBytes || (k.FetchFailed && retryFailed):
			p.fetch = append(p.fetch, m)
		case !k.ModifiedAt.Equal(m.ModifiedAt) || k.FolderPath != m.FolderPath:
			p.update = append(p.update, model.MetaUpdate{ID: k.ID, FolderPath: m.FolderPath, ModifiedAt: m.ModifiedAt, Importance: m.Importance})
		default:
			p.unchanged++
		}
	}

	gone := map[string][]int64{} // stored messages that could have been moved, by Message-ID and size
	var goneOther []int64
	for id, k := range known {
		if _, ok := current[id]; ok {
			continue
		}
		if k.InternetMessageID != "" {
			key := moveKey(k.InternetMessageID, k.SizeBytes)
			gone[key] = append(gone[key], k.ID)
		} else {
			goneOther = append(goneOther, k.ID)
		}
	}
	for _, ids := range gone {
		slices.Sort(ids)
	}

	// newest first, so that the mail people look for first becomes searchable first
	sort.Slice(added, func(i, j int) bool { return newer(added[i], added[j]) })
	for _, m := range added {
		if m.InternetMessageID != "" {
			key := moveKey(m.InternetMessageID, m.SizeBytes)
			if ids := gone[key]; len(ids) > 0 {
				p.move = append(p.move, model.MoveUpdate{ID: ids[0], EntryID: m.EntryID, FolderPath: m.FolderPath, ModifiedAt: m.ModifiedAt})
				gone[key] = ids[1:]
				continue
			}
		}
		p.fetch = append(p.fetch, m)
	}
	for _, ids := range gone {
		p.remove = append(p.remove, ids...)
	}
	p.remove = append(p.remove, goneOther...)

	sort.Slice(p.fetch, func(i, j int) bool { return newer(p.fetch[i], p.fetch[j]) })
	sort.Slice(p.update, func(i, j int) bool { return p.update[i].ID < p.update[j].ID })
	sort.Slice(p.move, func(i, j int) bool { return p.move[i].ID < p.move[j].ID })
	slices.Sort(p.remove)
	return p
}

func newer(a, b model.Meta) bool {
	if !a.ReceivedAt.Equal(b.ReceivedAt) {
		return a.ReceivedAt.After(b.ReceivedAt)
	}
	return a.EntryID < b.EntryID
}

// massDelete reports whether removing n of total stored messages looks like a missing store rather than a clean up:
// more than minCount messages and more than percent of the index.
func massDelete(n, total, percent, minCount int) bool {
	return n > minCount && n*100 > percent*total
}

func truncateRunes(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max { // a string of at most max bytes has at most max characters
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func filterFolders[T any](items []T, filter string, path func(T) string) []T {
	if filter == "" {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if containsFold(path(it), filter) {
			out = append(out, it)
		}
	}
	return out
}

func filterKnown(known map[string]model.Known, filter string) map[string]model.Known {
	if filter == "" {
		return known
	}
	out := make(map[string]model.Known, len(known))
	for id, k := range known {
		if containsFold(k.FolderPath, filter) {
			out[id] = k
		}
	}
	return out
}

func chunks[T any](s []T, n int, fn func([]T) error) error {
	for len(s) > 0 {
		k := min(n, len(s))
		if err := fn(s[:k]); err != nil {
			return err
		}
		s = s[k:]
	}
	return nil
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "..."
	}
	return s
}
