// Package config loads and validates the settings of maillens.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Config is the content of config\maillens.json. A key that the file leaves out keeps the value of Default.
type Config struct {
	Postgres Postgres `json:"postgres"`
	Outlook  Outlook  `json:"outlook"`
	Sync     Sync     `json:"sync"`
	Web      Web      `json:"web"`
	Search   Search   `json:"search"`
	Log      Log      `json:"log"`

	Path    string `json:"-"` // the file the settings came from
	BaseDir string `json:"-"` // relative paths in the settings are relative to this folder
}

// Postgres is the database that holds the index. The password may be left empty and given in the environment
// variable MAILLENS_PG_PASSWORD instead.
type Postgres struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
	Password string `json:"password"`
	SSLMode  string `json:"sslmode"`
	Schema   string `json:"schema"` // every table lives in this schema
}

// Outlook selects what is read from the local Outlook.
type Outlook struct {
	// Stores to index: "mailbox" is the primary Exchange mailbox, "pst" are the data files attached to the profile.
	Stores []string `json:"stores"`
	// ExcludeFolders are folder names or paths (without the store name, for example "Inbox\\Newsletters") that are left
	// out with everything below them. Deleted Items, Junk E-mail, Drafts and Outbox are always left out.
	ExcludeFolders []string `json:"exclude_folders"`
	// StartIfNeeded starts Outlook when it is not running. By default maillens only connects to a running Outlook.
	StartIfNeeded bool `json:"start_if_needed"`
}

// Sync controls the synchronisation of the index with the mailbox.
type Sync struct {
	IntervalMinutes int `json:"interval_minutes"` // between two runs of "maillens run"
	PauseMS         int `json:"pause_ms"`         // pause after every message read, to keep Outlook responsive
	BodyMaxChars    int `json:"body_max_chars"`   // longer bodies are cut
	BatchSize       int `json:"batch_size"`       // messages per database write
	// MirrorDeletions removes a message from the index when it is gone from Outlook, so that the index equals the
	// mailbox. A run that would remove more than MassDeletePercent of the index and more than MassDeleteMin
	// messages is refused unless "maillens sync -allow-mass-delete" is used: it points to a store that is missing.
	MirrorDeletions   bool `json:"mirror_deletions"`
	MassDeletePercent int  `json:"mass_delete_percent"`
	MassDeleteMin     int  `json:"mass_delete_min"`
}

// Web is the search page. It only listens on the loopback interface: the index holds the text of all mail.
type Web struct {
	Listen   string `json:"listen"`
	PageSize int    `json:"page_size"`
}

// Search tunes the queries.
type Search struct {
	// MaxCandidates is how many of the newest matches are ranked by relevance. Sorting by date is not limited.
	MaxCandidates int `json:"max_candidates"`
}

// Log is the log folder and level (debug, info, warn, error).
type Log struct {
	Dir   string `json:"dir"`
	Level string `json:"level"`
}

// Default returns the settings that a config file only has to override.
func Default() Config {
	return Config{
		Postgres: Postgres{Host: "127.0.0.1", Port: 5432, Database: "maillens", User: "maillens", SSLMode: "disable", Schema: "maillens"},
		Outlook:  Outlook{Stores: []string{"mailbox", "pst"}},
		Sync: Sync{
			IntervalMinutes: 10, BodyMaxChars: 2_000_000, BatchSize: 50,
			MirrorDeletions: true, MassDeletePercent: 10, MassDeleteMin: 1000,
		},
		Web:    Web{Listen: "127.0.0.1:8765", PageSize: 50},
		Search: Search{MaxCandidates: 5000},
		Log:    Log{Dir: "logs", Level: "info"},
	}
}

var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// DefaultPath is where the config file is looked up when -config is not given: config\maillens.json next to the
// program, or one folder above it (the program may sit in bin\), or in the current folder (go run).
func DefaultPath() string {
	rel := filepath.Join("config", "maillens.json")
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, base := range []string{dir, filepath.Dir(dir)} {
			p := filepath.Join(base, rel)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return rel
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config file %s does not exist: copy config\\maillens.example.json to config\\maillens.json and edit it", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	return parse(data, path)
}

func parse(data []byte, path string) (*Config, error) {
	cfg := Default()
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}) // a byte order mark, written by some editors
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // a misspelt key must not be ignored silently
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if v := os.Getenv("MAILLENS_PG_PASSWORD"); v != "" {
		cfg.Postgres.Password = v
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cfg.Path = abs
	cfg.BaseDir = filepath.Dir(abs)
	if strings.EqualFold(filepath.Base(cfg.BaseDir), "config") {
		cfg.BaseDir = filepath.Dir(cfg.BaseDir)
	}
	if cfg.Log.Dir != "" && !filepath.IsAbs(cfg.Log.Dir) {
		cfg.Log.Dir = filepath.Join(cfg.BaseDir, cfg.Log.Dir)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks the settings.
func (c *Config) Validate() error {
	p := c.Postgres
	switch {
	case strings.TrimSpace(p.Host) == "":
		return errors.New("postgres.host is empty")
	case p.Port < 1 || p.Port > 65535:
		return fmt.Errorf("postgres.port %d is out of range", p.Port)
	case strings.TrimSpace(p.Database) == "":
		return errors.New("postgres.database is empty")
	case strings.TrimSpace(p.User) == "":
		return errors.New("postgres.user is empty")
	case !identRe.MatchString(p.Schema):
		return fmt.Errorf("postgres.schema %q must be lower case letters, digits and underscores, starting with a letter or underscore", p.Schema)
	}
	switch p.SSLMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return fmt.Errorf("postgres.sslmode %q is not valid", p.SSLMode)
	}
	if len(c.Outlook.Stores) == 0 {
		return errors.New("outlook.stores is empty")
	}
	for _, s := range c.Outlook.Stores {
		if s != "mailbox" && s != "pst" {
			return fmt.Errorf(`outlook.stores: %q is not valid, use "mailbox" or "pst"`, s)
		}
	}
	s := c.Sync
	switch {
	case s.IntervalMinutes < 1:
		return errors.New("sync.interval_minutes must be at least 1")
	case s.PauseMS < 0:
		return errors.New("sync.pause_ms must not be negative")
	case s.BodyMaxChars < 1000:
		return errors.New("sync.body_max_chars must be at least 1000")
	case s.BatchSize < 1 || s.BatchSize > 1000:
		return errors.New("sync.batch_size must be between 1 and 1000")
	case s.MassDeletePercent < 1 || s.MassDeletePercent > 100:
		return errors.New("sync.mass_delete_percent must be between 1 and 100")
	case s.MassDeleteMin < 0:
		return errors.New("sync.mass_delete_min must not be negative")
	}
	if c.Web.PageSize < 1 || c.Web.PageSize > 200 {
		return errors.New("web.page_size must be between 1 and 200")
	}
	if err := checkLoopback(c.Web.Listen); err != nil {
		return fmt.Errorf("web.listen: %w", err)
	}
	if c.Search.MaxCandidates < 100 {
		return errors.New("search.max_candidates must be at least 100")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q is not valid", c.Log.Level)
	}
	return nil
}

// checkLoopback refuses an address that other computers could reach: the page shows the text of all mail and has no
// sign-in.
func checkLoopback(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("%q is not host:port", listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not valid", port)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q is not a loopback address: the page has no sign-in, so it only listens on 127.0.0.1, ::1 or localhost", host)
}

// DSN returns the whole database target as ONE connection string. Parse this string and nothing else: patching the
// host or port of an already parsed configuration leaves the fallback list of the driver on the defaults.
func (p Postgres) DSN() string {
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Path:   "/" + p.Database,
	}
	if p.Password != "" {
		u.User = url.UserPassword(p.User, p.Password)
	} else {
		u.User = url.User(p.User)
	}
	q := url.Values{}
	q.Set("sslmode", p.SSLMode)
	q.Set("application_name", "maillens")
	q.Set("connect_timeout", "10")
	u.RawQuery = q.Encode()
	return u.String()
}
