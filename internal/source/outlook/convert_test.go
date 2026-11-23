package outlook

import (
	"testing"
	"time"
)

func row(over map[int]interface{}) []interface{} {
	v := make([]interface{}, len(tableColumns))
	v[colEntryID] = "00000000ABCD"
	v[colSubject] = "Quarterly report"
	v[colReceived] = time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	v[colModified] = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	v[colSize] = int32(12345)
	v[colSenderName] = "Jane Doe"
	v[colSenderSMTP] = "jane.doe@example.com"
	v[colMessageID] = "<abc@example.com>"
	v[colHasAttach] = true
	v[colImportance] = int32(2)
	v[colTo] = "Me"
	v[colCc] = "Team"
	v[colClass] = "IPM.Note"
	for k, x := range over {
		v[k] = x
	}
	return v
}

func TestMetaFromRowConvertsTheValues(t *testing.T) {
	m, ok := MetaFromRow(row(nil), `box@x.com\Inbox`)
	if !ok {
		t.Fatal("a mail row must be accepted")
	}
	if m.EntryID != "00000000ABCD" || m.Subject != "Quarterly report" || m.SenderEmail != "jane.doe@example.com" ||
		m.SizeBytes != 12345 || m.Importance != 2 || !m.HasAttachments || m.FolderPath != `box@x.com\Inbox` ||
		m.InternetMessageID != "<abc@example.com>" || m.ToNames != "Me" || m.CcNames != "Team" {
		t.Errorf("unexpected meta: %+v", m)
	}
	if !m.ReceivedAt.Equal(time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)) || !m.ModifiedAt.Equal(time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("times: %v %v", m.ReceivedAt, m.ModifiedAt)
	}
}

func TestMetaFromRowSkipsWhatIsNotMail(t *testing.T) {
	for _, class := range []string{"IPM.Appointment", "IPM.Contact", "REPORT.IPM.Note.NDR", "IPM.Schedule.Meeting.Request"} {
		if _, ok := MetaFromRow(row(map[int]interface{}{colClass: class}), "f"); ok {
			t.Errorf("%s must be skipped", class)
		}
	}
	for _, class := range []string{"IPM.Note", "ipm.note.smime", "IPM.Note.Microsoft.Voicemail", ""} {
		if _, ok := MetaFromRow(row(map[int]interface{}{colClass: class}), "f"); !ok {
			t.Errorf("%q must be accepted", class)
		}
	}
	if _, ok := MetaFromRow(row(map[int]interface{}{colEntryID: ""}), "f"); ok {
		t.Error("a row without an entry id cannot be tracked")
	}
	if _, ok := MetaFromRow(make([]interface{}, 3), "f"); ok {
		t.Error("a short row must be rejected, not panic")
	}
}

func TestSenderAddressFallsBackAndDropsExchangeNames(t *testing.T) {
	m, _ := MetaFromRow(row(map[int]interface{}{colSenderSMTP: nil, colSenderEmail: "bob@corp.example"}), "f")
	if m.SenderEmail != "bob@corp.example" {
		t.Errorf("fallback address %q", m.SenderEmail)
	}
	m, _ = MetaFromRow(row(map[int]interface{}{colSenderSMTP: "", colSenderEmail: "/O=EXCHANGELABS/OU=X/CN=RECIPIENTS/CN=abc"}), "f")
	if m.SenderEmail != "" {
		t.Errorf("an Exchange distinguished name is not an address: %q", m.SenderEmail)
	}
}

func TestImportanceIsClamped(t *testing.T) {
	m, _ := MetaFromRow(row(map[int]interface{}{colImportance: int32(7)}), "f")
	if m.Importance != 1 {
		t.Errorf("importance %d", m.Importance)
	}
	m, _ = MetaFromRow(row(map[int]interface{}{colImportance: nil}), "f")
	if m.Importance != 1 {
		t.Errorf("a missing importance is normal, got %d", m.Importance)
	}
	m, _ = MetaFromRow(row(map[int]interface{}{colImportance: int32(0)}), "f")
	if m.Importance != 0 {
		t.Errorf("low importance must stay low, got %d", m.Importance)
	}
}

func TestAsTimeHandlesNoneAndOLEDates(t *testing.T) {
	if got := asTime(time.Date(4501, 1, 1, 0, 0, 0, 0, time.UTC)); !got.IsZero() {
		t.Errorf("the year 4501 means no date, got %v", got)
	}
	if got := asTime(nil); !got.IsZero() {
		t.Errorf("nil: %v", got)
	}
	// 45658 is 2025-01-01, so 46000.5 days after 1899-12-30 is day 343 of 2025: 2025-12-09 12:00
	want := time.Date(2025, 12, 9, 12, 0, 0, 0, time.UTC)
	if got := asTime(46000.5); !got.Equal(want) {
		t.Errorf("OLE date: got %v want %v", got, want)
	}
}

func TestClassifyStoreKeepsTheMailboxAndPSTFilesOnly(t *testing.T) {
	cases := []struct {
		typ  int
		path string
		kind string
		ok   bool
	}{
		{0, `C:\Users\x\AppData\Local\Microsoft\Outlook\x.ost`, "mailbox", true},
		{3, `D:\Mail\archive.pst`, "pst", true},
		{3, `D:\Mail\ARCHIVE.PST`, "pst", true},
		{3, `C:\Users\x\imap.ost`, "", false}, // an IMAP cache
		{1, ``, "", false},                    // shared mailbox or online archive
		{2, ``, "", false},                    // public folders
	}
	for _, c := range cases {
		kind, ok := ClassifyStore(c.typ, c.path)
		if kind != c.kind || ok != c.ok {
			t.Errorf("type %d %q: got %q %v, want %q %v", c.typ, c.path, kind, ok, c.kind, c.ok)
		}
	}
}

func TestStoreKeyIsStableAndCaseInsensitive(t *testing.T) {
	if StoreKey("pst", "Archive", `D:\Mail\Archive.PST`) != `pst:d:\mail\archive.pst` {
		t.Error("a pst is keyed by its path")
	}
	if StoreKey("mailbox", " Jane.Doe@Example.com ", "") != "mailbox:jane.doe@example.com" {
		t.Error("a mailbox is keyed by its name")
	}
}

func TestRelFolderPath(t *testing.T) {
	if got := RelFolderPath(`\\jane@x.com\Inbox\Reports`); got != `jane@x.com\Inbox\Reports` {
		t.Errorf("got %q", got)
	}
	if got := RelFolderPath(`Inbox`); got != "Inbox" {
		t.Errorf("got %q", got)
	}
}

func TestExcludedFolders(t *testing.T) {
	for _, n := range []string{"Sync Issues", "RSS Feeds", "conversation history", "Search Folders", " Team Chat "} {
		if !ExcludedByName(n) {
			t.Errorf("%q must be excluded", n)
		}
	}
	for _, n := range []string{"Inbox", "Sent Items", "Archive"} {
		if ExcludedByName(n) {
			t.Errorf("%q must not be excluded", n)
		}
	}
	pats := []string{"Newsletters", `Inbox\Alerts`}
	in := map[string]bool{
		`box@x.com\Inbox\Newsletters`:        true, // by name
		`box@x.com\Projects\newsletters`:     true, // by name, anywhere, case insensitive
		`box@x.com\Inbox\Alerts`:             true, // by path below the store
		`box@x.com\Inbox\Alerts\Critical`:    true, // below an excluded path
		`box@x.com\Inbox`:                    false,
		`box@x.com\Other\Alerts`:             false, // only Inbox\Alerts was named
		`box@x.com\Inbox\Alerts and Alarms`:  false, // a longer name is another folder
		`box@x.com\Inbox\Newsletters Weekly`: false,
	}
	for path, want := range in {
		if got := ExcludedByConfig(path, pats); got != want {
			t.Errorf("%s: got %v want %v", path, got, want)
		}
	}
	if ExcludedByConfig(`box@x.com\Inbox`, nil) {
		t.Error("no patterns exclude nothing")
	}
}

func TestJoinAttachmentNames(t *testing.T) {
	got := JoinAttachmentNames([]string{"report.pdf", "image001.png", "Report.PDF", " ", "data.xlsx", "image.png", "IMAGE002.JPG"})
	if want := "report.pdf; data.xlsx; image.png"; got != want {
		t.Errorf("got %q, want %q (signature pictures, repeats and blanks are dropped)", got, want)
	}
	if JoinAttachmentNames(nil) != "" {
		t.Error("no names, no text")
	}
}
