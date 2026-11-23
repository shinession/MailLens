// Command maillens copies the mail of the local Outlook into PostgreSQL and searches it there.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"maillens/internal/config"
	"maillens/internal/version"
)

const usage = `maillens %s - search your Outlook mail in PostgreSQL

Usage: maillens <command> [flags]

Commands:
  check      test the configuration, PostgreSQL and the connection to Outlook
  migrate    create or update the tables in PostgreSQL
  sync       copy mail from Outlook into PostgreSQL (the first run reads everything)
  serve      open the search page (http://127.0.0.1:8765/ unless configured otherwise)
  run        serve the page and sync every few minutes, until stopped with Ctrl+C
  search     search from the command line
  status     show what the index holds and how the last sync went
  version    print the version

Every command takes -config <file> (default: config\maillens.json next to the program).
Outlook can only be reached from a normal, non-administrator terminal: the one you start Outlook from.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, version.Version)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "maillens", version.Version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprintf(stdout, usage, version.Version)
		return 0
	case "check":
		return cmdCheck(rest, stdout, stderr)
	case "migrate":
		return cmdMigrate(rest, stdout, stderr)
	case "sync":
		return cmdSync(rest, stdout, stderr)
	case "serve":
		return cmdServe(rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdout, stderr)
	case "search":
		return cmdSearch(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
	fmt.Fprintf(stderr, usage, version.Version)
	return 2
}

// newFlags makes the flag set of a command with the -config flag that every command has.
func newFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", `config file (default: config\maillens.json next to the program)`)
	return fs, path
}

// parseFlags parses the arguments. ok is false when the command must stop with the given exit code.
func parseFlags(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		path = config.DefaultPath()
	}
	return config.Load(path)
}

// interruptContext ends on Ctrl+C or when the process is asked to stop.
func interruptContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// explainPG says what is wrong with a failed database connection, in the words people need.
func explainPG(err error, p config.Postgres) string {
	target := fmt.Sprintf("%s:%d database %q as user %q", p.Host, p.Port, p.Database, p.User)
	var pe *pgconn.PgError
	msg := err.Error()
	switch {
	case errors.As(err, &pe) && pe.Code == "28P01":
		return target + ": the password is wrong or missing. Set postgres.password in the config file, or the environment variable MAILLENS_PG_PASSWORD"
	case errors.As(err, &pe) && pe.Code == "28000":
		return fmt.Sprintf("%s: the server refused this user or host (check pg_hba.conf): %s", target, pe.Message)
	case errors.As(err, &pe) && pe.Code == "3D000":
		return target + ": the database does not exist. Create it first, see the README"
	case strings.Contains(msg, "actively refused") || strings.Contains(msg, "connection refused"):
		return target + ": PostgreSQL is not reachable. Is the service running, and is the port right?"
	}
	return fmt.Sprintf("%s: %v", target, err)
}

// commas writes a number with thousands separators.
func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.2f GB", float64(n)/(k*k*k))
}

func localTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
