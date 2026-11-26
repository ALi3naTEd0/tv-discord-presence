package store

import (
	"context"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
)

var now = time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC)

func newSession(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	dev, err := s.EnsureDevice(ctx, LocalUserID, "plex", "Plex", "")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.StartSession(ctx, dev, model.App{ID: "plex", Name: "Plex"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func movie(title string, year, tmdb int) model.Item {
	return model.Item{Identity: model.Identity{Kind: model.KindMovie, Title: title, Year: year, TMDB: tmdb}}
}

func setStatus(t *testing.T, s *Store, itemID int64, sink string, st model.DeliveryStatus) {
	t.Helper()
	if _, err := s.DB().Exec("UPDATE deliveries SET status = ? WHERE item_id = ? AND sink = ?", string(st), itemID, sink); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, s *Store, itemID int64, sink string) model.DeliveryStatus {
	t.Helper()
	d, err := s.DeliveryOf(context.Background(), itemID, sink)
	if err != nil {
		t.Fatal(err)
	}
	return d.Status
}

func items(t *testing.T, s *Store, sid int64) []model.Item {
	t.Helper()
	its, err := s.Items(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return its
}

var sinks = []string{"trakt", "simkl"}

func TestIdentifyCreatesPendingDeliveries(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	sid := newSession(t, s)
	if err := s.Identify(ctx, sid, []model.Item{movie("Dune", 2021, 438631)}, sinks, now); err != nil {
		t.Fatal(err)
	}
	its := items(t, s, sid)
	if len(its) != 1 || its[0].TMDB != 438631 {
		t.Fatalf("items %+v", its)
	}
	for _, sk := range sinks {
		if st := status(t, s, its[0].ID, sk); st != model.Pending {
			t.Errorf("%s: %s, want pending", sk, st)
		}
	}
}

// The wrong movie already reached Trakt but not Simkl: correcting it takes it
// out of Trakt, forgets Simkl's pending row, and sends the right one to both.
func TestCorrectionTakesWrongItemBackOut(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	sid := newSession(t, s)
	if err := s.Identify(ctx, sid, []model.Item{movie("So Long", 2025, 111)}, sinks, now); err != nil {
		t.Fatal(err)
	}
	wrong := items(t, s, sid)[0]
	setStatus(t, s, wrong.ID, "trakt", model.Added)

	if err := s.Identify(ctx, sid, []model.Item{movie("So Long", 2025, 222)}, sinks, now); err != nil {
		t.Fatal(err)
	}
	its := items(t, s, sid)
	if len(its) != 1 || its[0].TMDB != 222 {
		t.Fatalf("current items %+v, want only the corrected one", its)
	}
	if st := status(t, s, wrong.ID, "trakt"); st != model.Remove {
		t.Errorf("wrong item on Trakt: %s, want remove", st)
	}
	if _, err := s.DeliveryOf(ctx, wrong.ID, "simkl"); err == nil {
		t.Error("wrong item still has a Simkl delivery; it never got there")
	}
	for _, sk := range sinks {
		if st := status(t, s, its[0].ID, sk); st != model.Pending {
			t.Errorf("corrected item on %s: %s, want pending", sk, st)
		}
	}

	// Trakt confirms the removal: the replaced item goes away.
	if err := s.RemoveDone(ctx, wrong.ID, "trakt", now); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow("SELECT COUNT(*) FROM items WHERE id = ?", wrong.ID).Scan(&n)
	if n != 0 {
		t.Error("replaced item kept after every service removed it")
	}
}

func TestCorrectionToSameTitleSendsNothingAgain(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	sid := newSession(t, s)
	if err := s.Identify(ctx, sid, []model.Item{movie("Dune", 0, 438631)}, sinks, now); err != nil {
		t.Fatal(err)
	}
	it := items(t, s, sid)[0]
	setStatus(t, s, it.ID, "trakt", model.Added)
	fixed := movie("Dune: Part One", 2021, 438631)
	fixed.Poster = "https://image.tmdb.org/p.jpg"
	if err := s.Identify(ctx, sid, []model.Item{fixed}, sinks, now); err != nil {
		t.Fatal(err)
	}
	its := items(t, s, sid)
	if len(its) != 1 || its[0].ID != it.ID || its[0].Title != "Dune: Part One" || its[0].Year != 2021 || its[0].Poster == "" {
		t.Fatalf("items %+v, want the same row updated", its)
	}
	if st := status(t, s, it.ID, "trakt"); st != model.Added {
		t.Errorf("trakt %s, want still added", st)
	}
}

func TestCorrectionOfUnsentItemJustReplaces(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	sid := newSession(t, s)
	s.Identify(ctx, sid, []model.Item{movie("Wrong", 2020, 1)}, sinks, now)
	s.Identify(ctx, sid, []model.Item{movie("Right", 2020, 2)}, sinks, now)
	var n int
	s.DB().QueryRow("SELECT COUNT(*) FROM items").Scan(&n)
	if n != 1 {
		t.Errorf("%d item rows, want 1: an unsent wrong item needs no removal", n)
	}
}

// Changing which episodes a session held keeps the ones that stay.
func TestCorrectionOfEpisodes(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	sid := newSession(t, s)
	ep := func(n int) model.Item {
		return model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "The Curse", TMDB: 114655, Season: 1, Episode: n}}
	}
	s.Identify(ctx, sid, []model.Item{ep(1), ep(2)}, sinks, now)
	first := items(t, s, sid)
	setStatus(t, s, first[0].ID, "trakt", model.Added)
	setStatus(t, s, first[1].ID, "trakt", model.Added)
	if err := s.Identify(ctx, sid, []model.Item{ep(2), ep(3)}, sinks, now); err != nil {
		t.Fatal(err)
	}
	its := items(t, s, sid)
	if len(its) != 2 || its[0].ID != first[1].ID || its[1].Episode != 3 {
		t.Fatalf("items %+v, want E2 kept and E3 new", its)
	}
	if st := status(t, s, first[0].ID, "trakt"); st != model.Remove {
		t.Errorf("E1 on trakt %s, want remove", st)
	}
	if st := status(t, s, first[1].ID, "trakt"); st != model.Added {
		t.Errorf("E2 on trakt %s, want added", st)
	}
}

func TestIdentifyShowsShortSession(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	sid := newSession(t, s)
	s.DB().Exec("UPDATE sessions SET hidden = 'short' WHERE id = ?", sid)
	s.Identify(ctx, sid, []model.Item{movie("Short film", 2024, 9)}, sinks, now)
	got, _ := s.Session(ctx, sid)
	if got.Hidden != model.Shown {
		t.Errorf("hidden %q after identifying", got.Hidden)
	}
}

func TestNormalizeTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Les Enfants vont bien!": "les enfants vont bien",
		"  G'olé!  ":             "g ole",
		"Argentina — Campeones":  "argentina campeones",
		"WALL·E":                 "wall e",
		"Amélie":                 "amelie",
		"Laurie & Jessie":        "laurie and jessie",
		"":                       "",
	} {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAliases(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	target := model.Identity{Kind: model.KindMovie, Title: "Out of Love", Year: 2025, TMDB: 1234}
	if _, err := s.SaveAlias(ctx, model.Alias{UserID: LocalUserID, MatchTitle: "Les enfants vont bien", Identity: target}, now); err != nil {
		t.Fatal(err)
	}
	a, err := s.FindAlias(ctx, LocalUserID, "netflix", "LES ENFANTS VONT BIEN", 2025)
	if err != nil || a == nil || a.TMDB != 1234 || a.Hits != 1 {
		t.Fatalf("alias %+v, err %v", a, err)
	}

	// A more specific alias (this app, this year) wins over the general one.
	plex := model.Identity{Kind: model.KindMovie, Title: "Out of Love", Year: 2025, TMDB: 5678}
	if _, err := s.SaveAlias(ctx, model.Alias{UserID: LocalUserID, AppID: "plex", MatchTitle: "Les enfants vont bien", MatchYear: 2025, Identity: plex}, now); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.FindAlias(ctx, LocalUserID, "plex", "Les enfants vont bien", 2025); a == nil || a.TMDB != 5678 {
		t.Errorf("plex alias %+v, want the specific one", a)
	}
	if a, _ := s.FindAlias(ctx, LocalUserID, "plex", "Les enfants vont bien", 2019); a == nil || a.TMDB != 1234 {
		t.Errorf("other year %+v, want the general alias", a)
	}

	// Saving the same match again replaces the target.
	target.TMDB = 999
	s.SaveAlias(ctx, model.Alias{UserID: LocalUserID, MatchTitle: "les enfants vont bien!", Identity: target}, now)
	if a, _ := s.FindAlias(ctx, LocalUserID, "hbo", "Les enfants vont bien", 0); a == nil || a.TMDB != 999 {
		t.Errorf("after replacing: %+v", a)
	}

	if a, _ := s.FindAlias(ctx, LocalUserID, "netflix", "Something else", 0); a != nil {
		t.Errorf("unrelated title matched %+v", a)
	}
	if err := s.DeleteAlias(ctx, LocalUserID, a.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAliasNeedsAnID(t *testing.T) {
	s := openTemp(t)
	_, err := s.SaveAlias(context.Background(), model.Alias{UserID: LocalUserID, MatchTitle: "X",
		Identity: model.Identity{Kind: model.KindMovie, Title: "Y"}}, now)
	if err == nil {
		t.Error("alias without an external ID accepted; aliases resolve to IDs, never titles")
	}
}

func TestSameTitle(t *testing.T) {
	a := model.Identity{Kind: model.KindMovie, Title: "Dune", TMDB: 1, IMDb: "tt1"}
	for _, c := range []struct {
		b    model.Identity
		want bool
	}{
		{model.Identity{Kind: model.KindMovie, Title: "Dune: Part One", TMDB: 1}, true},
		{model.Identity{Kind: model.KindMovie, Title: "Dune", TMDB: 2}, false},
		{model.Identity{Kind: model.KindMovie, Title: "Other", IMDb: "tt1"}, true},
		{model.Identity{Kind: model.KindEpisode, Title: "Dune", TMDB: 1, Season: 1, Episode: 1}, false},
	} {
		if got := a.SameTitle(c.b); got != c.want {
			t.Errorf("SameTitle(%+v) = %v, want %v", c.b, got, c.want)
		}
	}
}

func TestAddPlay(t *testing.T) {
	s, ctx := openTemp(t), context.Background()
	dev, _ := s.EnsureDevice(ctx, LocalUserID, "plex", "Plex", "http://127.0.0.1:32400")
	p := source.Play{SourceKey: "/status/sessions/history/3", DeviceID: dev,
		StartedAt: now.Add(-69 * time.Minute), EndedAt: now,
		Identity: model.Identity{Kind: model.KindEpisode, Title: "The Curse", Season: 1, Episode: 10, TMDB: 114655}}
	added, err := s.AddPlay(ctx, p, "plex", "Plex", sinks, now)
	if err != nil || !added {
		t.Fatalf("added %v err %v", added, err)
	}
	if again, _ := s.AddPlay(ctx, p, "plex", "Plex", sinks, now); again {
		t.Error("the same play imported twice")
	}
	var sid int64
	s.DB().QueryRow("SELECT id FROM sessions WHERE source_key = ?", p.SourceKey).Scan(&sid)
	got, _ := s.Session(ctx, sid)
	if got.Open || got.Played != 69*time.Minute || !got.EndedAt.Equal(now) {
		t.Errorf("session %+v", got)
	}
	its := items(t, s, sid)
	if len(its) != 1 || its[0].Episode != 10 || status(t, s, its[0].ID, "trakt") != model.Pending {
		t.Errorf("items %+v", its)
	}
	// A play Plex could not identify (deleted from the library) waits for the picker.
	p2 := source.Play{SourceKey: "plex:99:1", DeviceID: dev, StartedAt: now.Add(-time.Hour), EndedAt: now,
		Identity: model.Identity{Kind: model.KindMovie, Title: "Deleted film", Year: 1999}}
	s.AddPlay(ctx, p2, "plex", "Plex", sinks, now)
	s.DB().QueryRow("SELECT id FROM sessions WHERE source_key = ?", p2.SourceKey).Scan(&sid)
	if its := items(t, s, sid); len(its) != 0 {
		t.Errorf("unidentified play got items %+v", its)
	}
}
