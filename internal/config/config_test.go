package config

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDefaultIsValid(t *testing.T) {
	d := Default()
	if err := d.Validate(); err != nil {
		t.Fatalf("the defaults must be valid: %v", err)
	}
}

func TestParseKeepsDefaultsForMissingKeysAndRejectsUnknownKeys(t *testing.T) {
	cfg, err := parse([]byte(`{"postgres": {"database": "mail2", "password": "pw"}}`), filepath.Join(t.TempDir(), "config", "maillens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Postgres.Database != "mail2" || cfg.Postgres.Host != "127.0.0.1" || cfg.Postgres.Port != 5432 || !cfg.Sync.MirrorDeletions {
		t.Errorf("defaults were not kept: %+v", cfg.Postgres)
	}
	if _, err := parse([]byte(`{"postgres": {"databse": "typo"}}`), "x.json"); err == nil || !strings.Contains(err.Error(), "databse") {
		t.Errorf("a misspelt key must be reported, got %v", err)
	}
}

func TestParseToleratesByteOrderMarkAndResolvesTheLogFolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config", "maillens.json")
	cfg, err := parse(append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{}`)...), path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "logs"); cfg.Log.Dir != want {
		t.Errorf("log dir %q, want %q (relative to the folder above config)", cfg.Log.Dir, want)
	}
}

func TestPasswordFromTheEnvironmentWins(t *testing.T) {
	t.Setenv("MAILLENS_PG_PASSWORD", "from-env")
	cfg, err := parse([]byte(`{"postgres": {"password": "from-file"}}`), "x.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Postgres.Password != "from-env" {
		t.Errorf("password %q", cfg.Postgres.Password)
	}
}

func TestValidateRejectsWhatCannotWork(t *testing.T) {
	cases := map[string]func(*Config){
		"schema with upper case": func(c *Config) { c.Postgres.Schema = "MailLens" },
		"schema with a quote":    func(c *Config) { c.Postgres.Schema = `a"b` },
		"bad sslmode":            func(c *Config) { c.Postgres.SSLMode = "bogus" },
		"port 0":                 func(c *Config) { c.Postgres.Port = 0 },
		"no stores":              func(c *Config) { c.Outlook.Stores = nil },
		"unknown store":          func(c *Config) { c.Outlook.Stores = []string{"imap"} },
		"interval 0":             func(c *Config) { c.Sync.IntervalMinutes = 0 },
		"mass delete percent 0":  func(c *Config) { c.Sync.MassDeletePercent = 0 },
		"public listen address":  func(c *Config) { c.Web.Listen = "0.0.0.0:8765" },
		"listen without port":    func(c *Config) { c.Web.Listen = "127.0.0.1" },
		"lan listen address":     func(c *Config) { c.Web.Listen = "192.168.1.5:8765" },
		"page size 0":            func(c *Config) { c.Web.PageSize = 0 },
		"bad log level":          func(c *Config) { c.Log.Level = "loud" },
	}
	for name, mutate := range cases {
		c := Default()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}

func TestLoopbackAddressesAreAccepted(t *testing.T) {
	for _, l := range []string{"127.0.0.1:8765", "localhost:8765", "[::1]:8765", "127.0.0.2:80"} {
		c := Default()
		c.Web.Listen = l
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", l, err)
		}
	}
}

// The password may hold characters that mean something in a URL. The whole target must survive the trip through the
// connection string and the parser of the driver, because it is the only place that builds the connection (a host or
// port patched into a parsed configuration would leave the fallbacks of the driver on localhost:5432).
func TestDSNSurvivesSpecialCharactersAndTheDriverParser(t *testing.T) {
	p := Default().Postgres
	p.Host, p.Port, p.Database, p.User, p.Password = "127.0.0.1", 55432, "mail db", "ma@il", `p@ss:w/rd#?%&=`
	dsn := p.DSN()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != p.Password || u.User.Username() != p.User {
		t.Errorf("credentials did not survive: %q / %q", u.User.Username(), pw)
	}

	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("the driver must accept the string: %v", err)
	}
	cc := pc.ConnConfig
	if cc.Host != "127.0.0.1" || cc.Port != 55432 || cc.Database != "mail db" || cc.User != "ma@il" || cc.Password != p.Password {
		t.Errorf("the driver read a different target: host=%s port=%d db=%q user=%q", cc.Host, cc.Port, cc.Database, cc.User)
	}
	for i, fb := range cc.Fallbacks {
		if fb.Host != "127.0.0.1" || fb.Port != 55432 {
			t.Errorf("fallback %d points to %s:%d instead of the configured server", i, fb.Host, fb.Port)
		}
	}
	if cc.RuntimeParams["application_name"] != "maillens" {
		t.Errorf("application_name %q", cc.RuntimeParams["application_name"])
	}
}

func TestLoadSaysWhatToDoWhenTheFileIsMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil || !strings.Contains(err.Error(), "maillens.example.json") {
		t.Errorf("the error must name the example file, got %v", err)
	}
	if _, statErr := os.Stat("config"); statErr == nil {
		t.Log("a config folder exists in the test directory, which is fine")
	}
}
