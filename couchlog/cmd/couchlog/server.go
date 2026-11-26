package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/compare"
	"github.com/ALi3naTEd0/couchlog/internal/config"
	"github.com/ALi3naTEd0/couchlog/internal/deliver"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/netflix"
	"github.com/ALi3naTEd0/couchlog/internal/server"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

func defaultConfig() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "couchlog", "config.toml")
}

func defaultTvpresenceConfig() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "tvpresence", "config.toml")
}

func initCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.Bool("from-tvpresence", false, "take the TV, box, keys and Plex from tvpresence's config.toml")
	out := fs.String("config", defaultConfig(), "where to write Couchlog's settings")
	force := fs.Bool("force", false, "overwrite existing settings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*from {
		return errors.New("only `couchlog init --from-tvpresence` exists for now (the setup assistant comes with the dashboard)")
	}
	if fileExists(*out) && !*force {
		return fmt.Errorf("%s already exists (use -force to overwrite)", *out)
	}
	src := defaultTvpresenceConfig()
	if fs.NArg() > 0 {
		src = fs.Arg(0)
	}
	c, err := config.FromTvpresence(src)
	if err != nil {
		return err
	}
	if err := c.Save(*out); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote %s: %d device(s)", *out, len(c.Devices))
	if c.Plex != nil {
		fmt.Fprintf(stdout, ", Plex at %s", c.Plex.URL)
	}
	fmt.Fprintln(stdout, ". Nothing is sent to Trakt or Simkl (no sinks).")
	return nil
}

func serverCmd(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig(), "settings file")
	debug := fs.Bool("debug", false, "verbose logs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	dbPath := cfg.DB
	if dbPath == "" {
		dbPath = defaultDB()
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("couchlog server", "version", version, "db", dbPath, "devices", len(cfg.Devices), "sinks", cfg.Sinks)
	srv := server.New(cfg, st, log)
	srv.DataDir = filepath.Dir(dbPath)
	return srv.Run(ctx)
}

func compareCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig(), "settings file (for the database path)")
	legacy := fs.String("legacy", defaultLegacy(), "tvpresence history.db")
	since := fs.Duration("since", 24*time.Hour, "how far back")
	diff := fs.Bool("diff", false, "only show sessions that differ")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbPath := defaultDB()
	if cfg, err := config.Load(*cfgPath); err == nil && cfg.DB != "" {
		dbPath = cfg.DB
	}
	r, err := compare.Run(ctx, dbPath, *legacy, time.Now().Add(-*since))
	if err != nil {
		return err
	}
	r.Write(stdout, *diff)
	return nil
}

func connectCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "netflix" {
		return errors.New("usage: couchlog connect netflix")
	}
	fs := flag.NewFlagSet("connect netflix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig(), "settings file (for where Couchlog keeps its data)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	dbPath := defaultDB()
	if cfg, err := config.Load(*cfgPath); err == nil && cfg.DB != "" {
		dbPath = cfg.DB
	}
	dir := filepath.Dir(dbPath)
	path := netflix.SessionPath(dir)
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	p, err := netflix.Connect(ctx, filepath.Join(dir, "netflix-browser"), path, func(s string) { fmt.Fprintln(stdout, s) })
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Saved in %s; Couchlog reads your Netflix activity with it from now on (%s).\n", path, p.Name)
	return nil
}

// checkCmd compares what Couchlog identified with your Trakt and Simkl
// history, read-only: it changes nothing anywhere.
func checkCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", defaultConfig(), "settings file")
	days := fs.Int("days", 7, "how far back")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if len(cfg.Sinks) == 0 {
		cfg.Sinks = []string{"trakt", "simkl"}
	}
	sinks, errs := deliver.Sinks(cfg)
	for _, e := range errs {
		fmt.Fprintln(stderr, "skipped:", e)
	}
	dbPath := cfg.DB
	if dbPath == "" {
		dbPath = defaultDB()
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	watches, err := st.Watches(ctx, time.Now().AddDate(0, 0, -*days), time.Now())
	if err != nil {
		return err
	}
	var entries []sink.Entry
	labels := map[int64]string{}
	for _, w := range watches {
		times := deliver.WatchedTimes(w)
		for _, it := range w.Items {
			entries = append(entries, sink.Entry{Item: it, WatchedAt: times[it.ID]})
			l := it.Title
			if it.Kind == model.KindEpisode {
				l += fmt.Sprintf(" S%dE%d", it.Season, it.Episode)
			}
			labels[it.ID] = time.UnixMilli(times[it.ID]).Format("01-02 15:04") + "  " + l
		}
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "Nothing identified in that time.")
		return nil
	}
	results := map[string]map[int64]sink.Result{}
	for _, sk := range sinks {
		res, err := sk.Check(ctx, entries)
		if err != nil {
			fmt.Fprintf(stdout, "%s: %v\n", sk.Name(), err)
			continue
		}
		results[sk.Name()] = map[int64]sink.Result{}
		for _, r := range res {
			results[sk.Name()][r.ItemID] = r
		}
	}
	for _, e := range entries {
		line := fmt.Sprintf("%-60.60s", labels[e.Item.ID])
		for _, sk := range sinks {
			if r, ok := results[sk.Name()][e.Item.ID]; ok {
				line += fmt.Sprintf("  %s: %-9s", sk.Name(), r.Status)
			}
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}
