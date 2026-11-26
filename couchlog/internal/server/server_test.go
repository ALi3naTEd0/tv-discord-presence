package server

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/config"
	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// script is a source that plays back observations, then waits.
type script struct {
	obs  []model.Observation
	done chan struct{}
}

func (s *script) Kind() string { return "script" }
func (s *script) Run(ctx context.Context, emit func(model.Observation)) error {
	for _, o := range s.obs {
		emit(o)
	}
	close(s.done)
	<-ctx.Done()
	return nil
}

type catalog struct{}

func (catalog) Search(_ context.Context, q string) ([]identify.Title, error) {
	if q == "Ozark" {
		return []identify.Title{{Kind: identify.Show, TMDB: 69740, Title: "Ozark"}}, nil
	}
	return nil, nil
}
func (catalog) Get(_ context.Context, k identify.Kind, id int) (identify.Title, error) {
	return identify.Title{Kind: k, TMDB: id, Title: "Ozark", Year: 2017, Runtime: 60,
		Seasons: []identify.Season{{Number: 1, Episodes: 10}}}, nil
}
func (catalog) Season(context.Context, int, int) ([]identify.Episode, error) {
	return []identify.Episode{{Number: 2, Name: "Sugarwood", Runtime: 58}}, nil
}
func (catalog) Find(context.Context, string, string) ([]identify.Title, error) { return nil, nil }

func TestServerRecordsAndIdentifies(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Now().Add(-10 * time.Minute)
	var obs []model.Observation
	for sec := 0; sec <= 300; sec += 5 {
		obs = append(obs, model.Observation{At: t0.Add(time.Duration(sec) * time.Second), State: model.StatePlaying,
			App: model.App{ID: "com.netflix.ninja", Name: "Netflix"}, MediaID: "Ozark",
			Hint: model.Hint{Title: "Ozark", Season: 1, Episode: 2}})
	}
	src := &script{obs: obs, done: make(chan struct{})}
	cfg := &config.Config{Devices: []config.Device{{Kind: "androidtv", Name: "Android TV", Host: "box"}}}
	s := &Server{Config: cfg, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ident:     &identify.Identifier{Catalog: catalog{}, Aliases: st},
		newSource: func(config.Device, int64) source.Source { return src }}
	go s.Run(ctx)
	<-src.done

	var sid int64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n, played int
		st.DB().QueryRow("SELECT (SELECT COUNT(*) FROM items), COALESCE((SELECT MAX(played_ms) FROM sessions), 0)").Scan(&n, &played)
		if n > 0 && played >= 300000 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := st.DB().QueryRow("SELECT id FROM sessions").Scan(&sid); err != nil {
		t.Fatal(err)
	}
	sess, _ := st.Session(ctx, sid)
	if !sess.Open || sess.Played < 290*time.Second || sess.App.ID != "com.netflix.ninja" {
		t.Errorf("session %+v", sess)
	}
	its, _ := st.Items(ctx, sid)
	if len(its) != 1 || its[0].TMDB != 69740 || its[0].Episode != 2 || its[0].EpisodeTitle != "Sugarwood" {
		t.Errorf("items %+v", its)
	}
	var dev string
	st.DB().QueryRow("SELECT kind || '/' || name FROM devices").Scan(&dev)
	if dev != "androidtv/Android TV" {
		t.Errorf("device %q", dev)
	}
}
