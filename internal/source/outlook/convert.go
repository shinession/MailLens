// Package outlook reads the local Outlook through COM. The COM calls are in com_windows.go and source_windows.go; this
// file holds the parts that need no COM and are tested on their own: the columns of the bulk table, the conversion of
// a table row, the choice of the stores and folders.
package outlook

import (
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"maillens/internal/model"
)

// tableColumns are the columns of the bulk table (Folder.GetTable) in the order of the values of a row. The names are
// DASL property names; PR_* values are MAPI property tags.
var tableColumns = []string{
	"EntryID",
	"urn:schemas:httpmail:subject",
	"urn:schemas:httpmail:datereceived",
	"urn:schemas:httpmail:datelastmodified",
	"http://schemas.microsoft.com/mapi/proptag/0x0E080003", // PR_MESSAGE_SIZE
	"urn:schemas:httpmail:sendername",
	"http://schemas.microsoft.com/mapi/proptag/0x5D01001F", // PR_SENDER_SMTP_ADDRESS, the address even for Exchange senders
	"urn:schemas:httpmail:senderemail",
	"http://schemas.microsoft.com/mapi/proptag/0x1035001F", // PR_INTERNET_MESSAGE_ID
	"urn:schemas:httpmail:hasattachment",
	"urn:schemas:httpmail:importance",
	"urn:schemas:httpmail:displayto",
	"urn:schemas:httpmail:displaycc",
	"http://schemas.microsoft.com/mapi/proptag/0x001A001F", // PR_MESSAGE_CLASS
}

const (
	colEntryID = iota
	colSubject
	colReceived
	colModified
	colSize
	colSenderName
	colSenderSMTP
	colSenderEmail
	colMessageID
	colHasAttach
	colImportance
	colTo
	colCc
	colClass
)

// MetaFromRow turns the values of one table row into a Meta. ok is false for a row that is not a mail item.
// The table returns times in UTC.
func MetaFromRow(vals []interface{}, folderPath string) (model.Meta, bool) {
	if len(vals) < len(tableColumns) {
		return model.Meta{}, false
	}
	if class := strings.TrimSpace(asString(vals[colClass])); class != "" && !strings.HasPrefix(strings.ToUpper(class), "IPM.NOTE") {
		return model.Meta{}, false
	}
	id := strings.TrimSpace(asString(vals[colEntryID]))
	if id == "" {
		return model.Meta{}, false
	}
	// The SMTP address property is empty for some items; the plain sender address is then an Exchange distinguished
	// name (/O=.../CN=...) that is useless for searching, so it is dropped.
	email := strings.TrimSpace(asString(vals[colSenderSMTP]))
	if !strings.Contains(email, "@") {
		email = strings.TrimSpace(asString(vals[colSenderEmail]))
		if !strings.Contains(email, "@") {
			email = ""
		}
	}
	imp := 1 // normal, also when the table has no value
	if vals[colImportance] != nil {
		if n := int(asInt64(vals[colImportance])); n >= 0 && n <= 2 {
			imp = n
		}
	}
	return model.Meta{
		EntryID:           id,
		FolderPath:        folderPath,
		InternetMessageID: strings.TrimSpace(asString(vals[colMessageID])),
		Subject:           asString(vals[colSubject]),
		SenderName:        asString(vals[colSenderName]),
		SenderEmail:       email,
		ToNames:           asString(vals[colTo]),
		CcNames:           asString(vals[colCc]),
		ReceivedAt:        asTime(vals[colReceived]),
		ModifiedAt:        asTime(vals[colModified]),
		SizeBytes:         asInt64(vals[colSize]),
		Importance:        imp,
		HasAttachments:    asBool(vals[colHasAttach]),
	}, true
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt64(v interface{}) int64 {
	switch x := v.(type) {
	case int32:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case int16:
		return int64(x)
	case int8:
		return int64(x)
	case uint32:
		return int64(x)
	case uint16:
		return int64(x)
	case uint8:
		return int64(x)
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	}
	return 0
}

func asBool(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int32:
		return x != 0
	case int64:
		return x != 0
	}
	return false
}

// asTime converts a time value of the table. Outlook uses the year 4501 for "no date".
func asTime(v interface{}) time.Time {
	switch x := v.(type) {
	case time.Time:
		if x.IsZero() || x.Year() < 1900 || x.Year() >= 4000 {
			return time.Time{}
		}
		return x.UTC()
	case float64: // an OLE automation date: days since 1899-12-30
		if x < 1 || x > admaxOLE {
			return time.Time{}
		}
		days := math.Floor(x)
		frac := x - days
		base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
		return base.AddDate(0, 0, int(days)).Add(time.Duration(frac * float64(24*time.Hour))).Round(time.Second)
	}
	return time.Time{}
}

// admaxOLE is the OLE automation date of the year 4000, from which a date means "none".
const admaxOLE = 745000

// ---- stores ----

// ClassifyStore decides whether a store is indexed and what kind it is. exchangeType is Outlook's
// OlExchangeStoreType: 0 primary mailbox, 1 another mailbox (a shared mailbox or the online archive), 2 public
// folders, 3 not Exchange. Only the primary mailbox and data files (.pst) are indexed.
func ClassifyStore(exchangeType int, filePath string) (kind string, ok bool) {
	switch exchangeType {
	case 0:
		return "mailbox", true
	case 3:
		if strings.EqualFold(filepath.Ext(strings.TrimSpace(filePath)), ".pst") {
			return "pst", true
		}
	}
	return "", false
}

// StoreKey is the stable key of a store in the database: the mailbox name, or the path of the .pst file.
func StoreKey(kind, displayName, filePath string) string {
	if kind == "pst" {
		return "pst:" + strings.ToLower(strings.TrimSpace(filePath))
	}
	return kind + ":" + strings.ToLower(strings.TrimSpace(displayName))
}

// ---- folders ----

// RelFolderPath removes the leading "\\" of an Outlook folder path ("\\name\Inbox" becomes "name\Inbox").
func RelFolderPath(p string) string {
	return strings.TrimPrefix(p, `\\`)
}

// alwaysExcludedNames are folders that hold no mail worth searching: system folders, sync logs, search folders and
// the Teams chat copies.
var alwaysExcludedNames = map[string]bool{
	"sync issues": true, "conflicts": true, "local failures": true, "server failures": true,
	"rss feeds": true, "conversation history": true, "search folders": true, "yammer root": true,
	"quick step settings": true, "suggested contacts": true, "recipient cache": true, "files": true,
	"externalcontacts": true, "personmetadata": true, "team chat": true, "social activity notifications": true,
}

// ExcludedByName reports whether a folder name is one of the always excluded system folders.
func ExcludedByName(name string) bool {
	return alwaysExcludedNames[strings.ToLower(strings.TrimSpace(name))]
}

// ExcludedByConfig reports whether the exclude_folders setting leaves a folder out. rel is the folder path as Outlook
// writes it, starting with the store name. A pattern matches a folder by its name wherever it is, or by its path
// below the store ("Inbox\Newsletters"); everything below a matching folder is left out as well.
func ExcludedByConfig(rel string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	within := rel
	if i := strings.Index(rel, `\`); i >= 0 {
		within = rel[i+1:]
	}
	name := rel[strings.LastIndex(rel, `\`)+1:]
	lowWithin := strings.ToLower(within)
	for _, p := range patterns {
		p = strings.Trim(strings.TrimSpace(p), `\`)
		if p == "" {
			continue
		}
		lp := strings.ToLower(p)
		if strings.EqualFold(name, p) || lowWithin == lp || strings.ToLower(rel) == lp || strings.HasPrefix(lowWithin, lp+`\`) {
			return true
		}
	}
	return false
}

// inlineImageRe matches the numbered pictures Outlook adds for signatures and pasted images.
var inlineImageRe = regexp.MustCompile(`(?i)^image\d{3}\.(png|jpe?g|gif|bmp)$`)

// JoinAttachmentNames joins attachment file names with "; ", leaving out empty names, signature pictures and
// repeats.
func JoinAttachmentNames(names []string) string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || inlineImageRe.MatchString(n) {
			continue
		}
		k := strings.ToLower(n)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, n)
	}
	return strings.Join(out, "; ")
}
