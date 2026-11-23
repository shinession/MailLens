// Package logx builds the logger: standard error plus one file per day.
package logx

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// daily writes to maillens-YYYYMMDD.log in a folder and switches the file at midnight.
type daily struct {
	mu  sync.Mutex
	dir string
	day string
	f   *os.File
}

func (d *daily) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	day := time.Now().Format("20060102")
	if d.f == nil || day != d.day {
		if d.f != nil {
			_ = d.f.Close()
			d.f = nil
		}
		f, err := os.OpenFile(filepath.Join(d.dir, "maillens-"+day+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return len(p), nil // logging must never stop the program; standard error still has the line
		}
		d.f, d.day = f, day
	}
	return d.f.Write(p)
}

func (d *daily) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

func levelOf(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// Console returns a logger that only writes to standard error, for the commands that print their own result.
func Console(level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: levelOf(level)}))
}

// New returns a logger that writes to standard error and to the daily file in dir. Close the returned value when done.
func New(dir, level string) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	d := &daily{dir: dir}
	h := slog.NewTextHandler(io.MultiWriter(os.Stderr, d), &slog.HandlerOptions{Level: levelOf(level)})
	return slog.New(h), d, nil
}
