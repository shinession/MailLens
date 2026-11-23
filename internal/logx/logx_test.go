package logx

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewWritesTheDailyFileAndHonoursTheLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs", "deeper") // the folder does not exist yet
	log, closer, err := New(dir, "warn")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("below the level")
	log.Warn("above the level", "count", 3)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}

	name := filepath.Join(dir, "maillens-"+time.Now().Format("20060102")+".log")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("the daily file must exist: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "above the level") || !strings.Contains(text, "count=3") {
		t.Errorf("the warning is in the file: %q", text)
	}
	if strings.Contains(text, "below the level") {
		t.Errorf("an info line must not be written at level warn: %q", text)
	}
}

func TestAnUnwritableFolderNeverStopsTheProgram(t *testing.T) {
	d := &daily{dir: filepath.Join(t.TempDir(), "does", "not", "exist")} // the folder is missing: opening fails
	n, err := d.Write([]byte("a line\n"))
	if err != nil || n != len("a line\n") {
		t.Errorf("logging must swallow the failure, got %d %v", n, err)
	}
	_ = d.Close()
}

func TestLevelOf(t *testing.T) {
	cases := map[string]slog.Level{"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "Warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo, "loud": slog.LevelInfo}
	for in, want := range cases {
		if got := levelOf(in); got != want {
			t.Errorf("levelOf(%q) = %v, want %v", in, got, want)
		}
	}
}
