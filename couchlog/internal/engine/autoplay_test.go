package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

func episode(n, runtime int, estimated bool) model.Item {
	return model.Item{Identity: model.Identity{Kind: model.KindEpisode, Title: "The Curse", TMDB: 114655, Season: 1, Episode: n},
		RuntimeMinutes: runtime, RuntimeEstimated: estimated}
}

// autoplayRig is an Android TV box whose tracker knows the next episode.
func autoplayRig(t *testing.T) *rig {
	r := newRig(t, true)
	r.tr.Sinks = []string{"trakt"}
	r.tr.Next = func(_ context.Context, cur model.Item) (model.Item, error) {
		return episode(cur.Episode+1, 30, false), nil
	}
	return r
}

// playAt plays from..to with the real position following the clock from start.
func (r *rig) playPos(from, to, start int) *model.Session {
	r.t.Helper()
	var s *model.Session
	for sec := from; sec <= to; sec += 5 {
		s = r.at(sec, model.StatePlaying, netflix, media("The Curse"), pos(time.Duration(start+sec-from)*time.Second))
	}
	return s
}

func (r *rig) identify(sid int64, it model.Item) {
	r.t.Helper()
	if err := r.st.Identify(r.ctx, sid, []model.Item{it}, []string{"trakt"}, t0); err != nil {
		r.t.Fatal(err)
	}
}

func TestAutoplaySplitsOntoNextEpisode(t *testing.T) {
	r := autoplayRig(t)
	s := r.playPos(0, 60, 0)
	r.identify(s.ID, episode(1, 30, false))
	r.playPos(65, 1700, 65) // up to 28 min of a 30 min episode
	next := r.playPos(1705, 1800, 8)

	ss := r.wantSessions(2)
	first, second := ss[0], ss[1]
	if first.Open || second.ID != next.ID || !second.Open {
		t.Fatalf("first open=%v, second %d open=%v", first.Open, second.ID, second.Open)
	}
	if second.AutoNextFrom == nil || *second.AutoNextFrom != first.ID {
		t.Errorf("auto_next_from %v, want %d", second.AutoNextFrom, first.ID)
	}
	its, _ := r.st.Items(r.ctx, second.ID)
	if len(its) != 1 || its[0].Episode != 2 {
		t.Fatalf("next session items %+v", its)
	}
	if d, err := r.st.DeliveryOf(r.ctx, its[0].ID, "trakt"); err != nil || d.Status != model.Pending {
		t.Errorf("next episode delivery %+v %v", d, err)
	}
	if second.Played < 90*time.Second || first.Played > 1700*time.Second {
		t.Errorf("played: first %v, second %v", first.Played, second.Played)
	}
	if r.events[len(r.events)-1].Kind != AutoNext {
		t.Errorf("last event %+v, want next", r.events[len(r.events)-1])
	}

	// Undo: it was the same episode from the start; the time goes back.
	prev, err := r.st.UndoNext(r.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !prev.Open || prev.Played < first.Played+90*time.Second {
		t.Errorf("after undo: open=%v played %v", prev.Open, prev.Played)
	}
	r.wantSessions(1)
	if _, err := r.st.UndoNext(r.ctx, prev.ID); !errors.Is(err, store.ErrNotAutoSplit) {
		t.Errorf("undo of a session that was not split: %v", err)
	}
}

func TestAutoplayNeedsRealWatching(t *testing.T) {
	for name, c := range map[string]struct {
		item       model.Item
		watchUntil int // last second played before the jump
		jumpTo     int
	}{
		"not past 80 %":       {episode(1, 30, false), 1300, 5},
		"estimated runtime":   {episode(1, 30, true), 1700, 5},
		"unknown runtime":     {episode(1, 0, false), 1700, 5},
		"movie":               {model.Item{Identity: model.Identity{Kind: model.KindMovie, Title: "M", TMDB: 1}, RuntimeMinutes: 30}, 1700, 5},
		"restart not at zero": {episode(1, 30, false), 1700, 400},
	} {
		t.Run(name, func(t *testing.T) {
			r := autoplayRig(t)
			s := r.playPos(0, 60, 0)
			r.identify(s.ID, c.item)
			r.playPos(65, c.watchUntil, 65)
			r.playPos(c.watchUntil+5, c.watchUntil+60, c.jumpTo)
			r.wantSessions(1)
		})
	}
}

// Netflix went back to an earlier episode while you were away and you
// skipped to its end: little was played, so that is not this one finishing.
func TestAutoplayIgnoresSkipToEnd(t *testing.T) {
	r := autoplayRig(t)
	s := r.playPos(0, 60, 0)
	r.identify(s.ID, episode(1, 30, false))
	r.playPos(65, 120, 1600) // seeked near the end: 2 min played in total
	r.playPos(125, 200, 5)
	r.wantSessions(1)
}

func TestAutoplayWithoutNextEpisodeKeepsSession(t *testing.T) {
	r := autoplayRig(t)
	r.tr.Next = func(context.Context, model.Item) (model.Item, error) { return model.Item{}, errors.New("TMDB down") }
	s := r.playPos(0, 60, 0)
	r.identify(s.ID, episode(1, 30, false))
	r.playPos(65, 1700, 65)
	r.playPos(1705, 1800, 8)
	if ss := r.wantSessions(1); !ss[0].Open {
		t.Error("session closed although the split failed")
	}
}

// The episode picked up again near its end (an earlier session had most of
// it): playing on to the next episode still splits.
func TestAutoplayAfterPickingUpAgain(t *testing.T) {
	r := autoplayRig(t)
	s := r.playPos(0, 60, 1500) // resumes at 25 min of a 30 min episode
	r.identify(s.ID, episode(1, 30, false))
	r.playPos(65, 300, 1565) // plays on to ~29:55
	r.playPos(305, 400, 5)   // the next one starts
	ss := r.wantSessions(2)
	if ss[1].AutoNextFrom == nil {
		t.Error("no split after picking the episode up again")
	}
}
