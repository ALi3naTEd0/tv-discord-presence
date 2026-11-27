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
