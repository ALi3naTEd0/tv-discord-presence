// Command couchlog is your own TV watch history.
//
//	couchlog init --from-tvpresence [config.toml]   settings from tvpresence's (same TV, box, keys)
//	couchlog server [-config path]                  listen to the TVs, import Plex
//	couchlog compare [-since 24h] [-diff]           Go vs tvpresence, session by session
//	couchlog connect netflix                        sign in to Netflix in Couchlog's own window
//	couchlog check [-days 7]                        what Trakt and Simkl already have (read-only)
//	couchlog import [-db path] [history.db]         bring in a tvpresence history
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ALi3naTEd0/couchlog/internal/store"
)

var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "couchlog:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: couchlog init | server | compare | connect netflix | check | import | version")
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "couchlog", version)
		return nil
	case "import":
		return importCmd(ctx, args[1:], stdout, stderr)
	case "init":
		return initCmd(args[1:], stdout, stderr)
	case "server":
		return serverCmd(ctx, args[1:], stderr)
	case "compare":
		return compareCmd(ctx, args[1:], stdout, stderr)
	case "connect":
		return connectCmd(ctx, args[1:], stdout, stderr)
	case "check":
		return checkCmd(ctx, args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func defaultDB() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "couchlog", "couchlog.db")
}

// defaultLegacy is where tvpresence keeps its history on this system.
func defaultLegacy() string {
	if d := os.Getenv("TVPRESENCE_DATA_DIR"); d != "" {
		return filepath.Join(d, "history.db")
	}
	home, _ := os.UserHomeDir()
	switch {
	case os.Getenv("XDG_DATA_HOME") != "":
		return filepath.Join(os.Getenv("XDG_DATA_HOME"), "tvpresence", "history.db")
	case os.PathSeparator == '\\':
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "tvpresence", "history.db")
	case fileExists(filepath.Join(home, "Library", "Application Support", "tvpresence", "history.db")):
		return filepath.Join(home, "Library", "Application Support", "tvpresence", "history.db")
	}
	return filepath.Join(home, ".local", "share", "tvpresence", "history.db")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func importCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDB(), "Couchlog database to import into")
	if err := fs.Parse(args); err != nil {
		return err
	}
	legacy := defaultLegacy()
	if fs.NArg() > 0 {
		legacy = fs.Arg(0)
	}
	if !fileExists(legacy) {
		return fmt.Errorf("no tvpresence history at %s", legacy)
	}
	s, err := store.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := s.ImportTvpresence(ctx, legacy)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Imported %d sessions, %d titles and %d Trakt/Simkl statuses from %s into %s",
		st.Sessions, st.Items, st.Deliveries, legacy, *dbPath)
	if st.Skipped > 0 {
		fmt.Fprintf(stdout, " (%d already imported)", st.Skipped)
	}
	fmt.Fprintln(stdout)
	return nil
}
