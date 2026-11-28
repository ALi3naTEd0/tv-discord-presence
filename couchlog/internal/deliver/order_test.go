package deliver

import (
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

func TestWatchedInOrder(t *testing.T) {
	d := &Deliverer{}
	w := func(pos, played time.Duration, guessed bool) store.Watch {
		p := pos
		return store.Watch{Session: &model.Session{Position: &p, Played: played}, DeviceKind: "androidtv",
			Items: []model.Item{{Identity: model.Identity{Kind: model.KindEpisode, TMDB: 340053, Season: 1, Episode: 1}, RuntimeMinutes: 66, Guessed: guessed}}}
	}
	if _, ok := d.WatchedInOrder(w(64*time.Minute, 65*time.Minute, true)); !ok {
		t.Error("watched from the start to the end: should go")
	}
	if _, ok := d.WatchedInOrder(w(64*time.Minute, 20*time.Minute, true)); ok {
		t.Error("jumped ahead: 44 min skipped, not the next episode watched in order")
	}
	if _, ok := d.WatchedInOrder(w(30*time.Minute, 30*time.Minute, true)); ok {
		t.Error("half of it: not watched")
	}
	if _, ok := d.WatchedInOrder(w(64*time.Minute, 65*time.Minute, false)); ok {
		t.Error("not a guess: nothing to confirm")
	}
}
