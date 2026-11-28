package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

func TestSubscriptionsAndSettings(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.SetSubscription(ctx, LocalUserID, "netflix", &Subscription{PriceCents: 29900, Currency: "MXN", Period: "month"}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscription(ctx, LocalUserID, "hbomax", &Subscription{PriceCents: 100, Currency: "mxn", Period: "month"}, now); err == nil {
		t.Error("lower-case currency accepted")
	}
	subs, _ := s.Subscriptions(ctx, LocalUserID)
	if subs["netflix"].PriceCents != 29900 || len(subs) != 1 {
		t.Errorf("%+v", subs)
	}
	s.SetSubscription(ctx, LocalUserID, "netflix", nil, now)
	if subs, _ = s.Subscriptions(ctx, LocalUserID); len(subs) != 0 {
		t.Errorf("not removed: %+v", subs)
	}
	if v, _ := s.Setting(ctx, LocalUserID, "currency", "MXN"); v != "MXN" {
		t.Errorf("default %q", v)
	}
	s.SetSetting(ctx, LocalUserID, "currency", "USD")
	if v, _ := s.Setting(ctx, LocalUserID, "currency", "MXN"); v != "USD" {
		t.Errorf("saved %q", v)
	}
	dev, _ := s.EnsureDevice(ctx, LocalUserID, "androidtv", "Box", "")
	sess, _ := s.StartSession(ctx, dev, model.App{ID: "x"}, "", now)
	if err := s.SetHidden(ctx, sess.ID, model.Ignored); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Session(ctx, sess.ID); got.Hidden != model.Ignored {
		t.Errorf("hidden %q", got.Hidden)
	}
	if err := s.SetHidden(ctx, 999, model.Shown); err != ErrNoSession {
		t.Errorf("missing: %v", err)
	}
}

func TestSplitNextAtMovesTheTimeSince(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Date(2026, 11, 26, 20, 27, 0, 0, time.UTC)
	dev, _ := s.EnsureDevice(ctx, LocalUserID, "androidtv", "Box", "")
	sess, _ := s.StartSession(ctx, dev, model.App{ID: "com.netflix.ninja"}, "", t0)
	s.Touch(ctx, sess.ID, t0.Add(41*time.Minute), 41*time.Minute, true)
	next := model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "Sins of Our Mother", TMDB: 1, Season: 1, Episode: 3}}
	// the new title started at 21:03; Netflix listed it at 21:08
	ns, err := s.SplitNextAt(ctx, sess.ID, next, nil, t0.Add(36*time.Minute), t0.Add(41*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := s.Session(ctx, sess.ID)
	if !old.EndedAt.Equal(t0.Add(36*time.Minute)) || old.Played != 36*time.Minute {
		t.Errorf("old: ended %v played %v", old.EndedAt, old.Played)
	}
	if !ns.StartedAt.Equal(t0.Add(36*time.Minute)) || ns.Played != 5*time.Minute || !ns.Open {
		t.Errorf("new: started %v played %v open %v", ns.StartedAt, ns.Played, ns.Open)
	}
}

func TestEnsureDeliveriesForTitlesFromBeforeASink(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	dev, _ := s.EnsureDevice(ctx, LocalUserID, "plex", "Plex", "")
	sess, _ := s.StartSession(ctx, dev, model.App{ID: "plex"}, "", now.Add(-48*time.Hour))
	it := model.Item{Identity: model.Identity{Kind: model.KindMovie, Title: "Listen", TMDB: 9}}
	s.Identify(ctx, sess.ID, []model.Item{it}, nil, now) // recorded with no destinations
	n, err := s.EnsureDeliveries(ctx, []string{"trakt", "simkl"}, now.AddDate(0, 0, -60), now)
	if err != nil || n != 2 {
		t.Fatalf("added %d, %v", n, err)
	}
	items, _ := s.Items(ctx, sess.ID)
	if d, err := s.DeliveryOf(ctx, items[0].ID, "trakt"); err != nil || d.Status != model.Pending {
		t.Errorf("%+v %v", d, err)
	}
	if n, _ := s.EnsureDeliveries(ctx, []string{"trakt", "simkl"}, now.AddDate(0, 0, -60), now); n != 0 {
		t.Errorf("added again: %d", n)
	}
}

func TestGuessesStayUnsentUntilConfirmed(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	dev, _ := s.EnsureDevice(ctx, LocalUserID, "androidtv", "Box", "")
	sess, _ := s.StartSession(ctx, dev, model.App{ID: "com.disney.disneyplus"}, "", now.Add(-time.Hour))
	guess := model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "Get Huge", TMDB: 340053, Season: 1, Episode: 2}, Guessed: true}
	s.Identify(ctx, sess.ID, []model.Item{guess}, []string{"trakt"}, now)
	if n, _ := s.EnsureDeliveries(ctx, []string{"trakt"}, now.AddDate(0, 0, -60), now); n != 0 {
		t.Errorf("a guess got %d deliveries", n)
	}
	items, _ := s.Items(ctx, sess.ID)
	if len(items) != 1 || !items[0].Guessed {
		t.Fatalf("%+v", items)
	}
	// you confirm it: the same title identified by you
	guess.Guessed = false
	s.Identify(ctx, sess.ID, []model.Item{guess}, []string{"trakt"}, now)
	items, _ = s.Items(ctx, sess.ID)
	if items[0].Guessed {
		t.Error("still a guess after confirming")
	}
	if n, _ := s.EnsureDeliveries(ctx, []string{"trakt"}, now.AddDate(0, 0, -60), now); n != 1 {
		t.Errorf("confirmed item got %d deliveries", n)
	}
}
