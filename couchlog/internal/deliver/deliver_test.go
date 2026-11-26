package deliver

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/sink"
	"github.com/ALi3naTEd0/couchlog/internal/source"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// memSink is a destination in memory: has = already in your history.
type memSink struct {
	has             map[int]bool // by TMDB id
	pushed, removed []int64
	checked         int
}

func (m *memSink) Name() string { return "trakt" }
func (m *memSink) Check(_ context.Context, es []sink.Entry) ([]sink.Result, error) {
	var out []sink.Result
	for _, e := range es {
		m.checked++
		st := model.Ready
		if m.has[e.Item.TMDB] {
			st = model.Added
		}
		out = append(out, sink.Result{ItemID: e.Item.ID, Status: st})
	}
	return out, nil
}
func (m *memSink) Push(_ context.Context, es []sink.Entry) ([]sink.Result, error) {
	var out []sink.Result
	for _, e := range es {
		m.pushed = append(m.pushed, e.Item.ID)
		out = append(out, sink.Result{ItemID: e.Item.ID, Status: model.Added})
	}
	return out, nil
}
func (m *memSink) Remove(_ context.Context, es []sink.Entry) ([]sink.Result, error) {
	var out []sink.Result
	for _, e := range es {
		m.removed = append(m.removed, e.Item.ID)
		out = append(out, sink.Result{ItemID: e.Item.ID, Status: model.Removed})
	}
	return out, nil
}

var t0 = time.Now().Add(-3 * time.Hour)

type rig struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	box int64
	px  int64
}

func newRig(t *testing.T) *rig {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Box", "")
	px, _ := st.EnsureDevice(ctx, store.LocalUserID, "plex", "Plex", "")
	return &rig{t, ctx, st, box, px}
}

// tvSession plays minutes of a title with the given runtime.
func (r *rig) tvSession(tmdb, runtime, minutes int, estimated bool) int64 {
	s, _ := r.st.StartSession(r.ctx, r.box, model.App{ID: "com.netflix.ninja"}, "", t0)
	r.st.Touch(r.ctx, s.ID, t0.Add(time.Duration(minutes)*time.Minute), time.Duration(minutes)*time.Minute, true)
	r.st.CloseSession(r.ctx, s.ID, 0, 0)
	r.st.Identify(r.ctx, s.ID, []model.Item{{Identity: model.Identity{Kind: model.KindMovie, Title: "m", TMDB: tmdb},
		RuntimeMinutes: runtime, RuntimeEstimated: estimated}}, []string{"trakt"}, t0)
	its, _ := r.st.Items(r.ctx, s.ID)
	return its[0].ID
}

func (r *rig) status(id int64) model.DeliveryStatus {
	d, err := r.st.DeliveryOf(r.ctx, id, "trakt")
	if err != nil {
		r.t.Fatal(err)
	}
	return d.Status
}

func TestRules(t *testing.T) {
	r := newRig(t)
	full := r.tvSession(1, 100, 95, false)    // watched: sent
	half := r.tvSession(2, 100, 50, false)    // half: waits
	est := r.tvSession(3, 100, 100, true)     // estimated runtime: waits
	already := r.tvSession(4, 100, 90, false) // already in your history: marked, not sent
	r.st.AddPlay(r.ctx, source.Play{SourceKey: "p1", DeviceID: r.px, StartedAt: t0, EndedAt: t0.Add(time.Minute),
		Identity: model.Identity{Kind: model.KindMovie, Title: "p", TMDB: 5}}, "plex", "Plex", []string{"trakt"}, t0)
	m := &memSink{has: map[int]bool{4: true}}
	d := &Deliverer{Store: r.st, Sinks: []sink.Sink{m}}
	if _, err := d.Run(r.ctx); err != nil {
		t.Fatal(err)
	}
	if r.status(full) != model.Added || r.status(already) != model.Added || r.status(half) != model.Pending || r.status(est) != model.Pending {
		t.Errorf("statuses full=%s already=%s half=%s est=%s", r.status(full), r.status(already), r.status(half), r.status(est))
	}
	if len(m.pushed) != 2 { // full + the Plex play (Plex is sent as it is)
		t.Errorf("pushed %v", m.pushed)
	}
	del, _ := r.st.DeliveryOf(r.ctx, half, "trakt")
	if del.Error == "" {
		t.Error("no note on the half-watched session")
	}
	// A second run sends nothing again.
	before := len(m.pushed)
	d.Run(r.ctx)
	if len(m.pushed) != before {
		t.Errorf("sent twice: %v", m.pushed)
	}
}

func TestReadOnlyOnlyChecks(t *testing.T) {
	r := newRig(t)
	sent := r.tvSession(1, 100, 95, false)
	notYet := r.tvSession(2, 100, 95, false)
	m := &memSink{has: map[int]bool{1: true}}
	d := &Deliverer{Store: r.st, Sinks: []sink.Sink{m}, ReadOnly: true}
	d.Run(r.ctx)
	if r.status(sent) != model.Added || r.status(notYet) != model.Ready || len(m.pushed) != 0 {
		t.Errorf("sent=%s notYet=%s pushed=%v", r.status(sent), r.status(notYet), m.pushed)
	}
	// Later the other app sends it: the next check marks it.
	m.has[2] = true
	d.Run(r.ctx)
	if r.status(notYet) != model.Added || len(m.pushed) != 0 {
		t.Errorf("after: %s pushed %v", r.status(notYet), m.pushed)
	}
}

func TestCorrectionIsTakenOut(t *testing.T) {
	r := newRig(t)
	wrong := r.tvSession(1, 100, 95, false)
	m := &memSink{has: map[int]bool{}}
	d := &Deliverer{Store: r.st, Sinks: []sink.Sink{m}}
	d.Run(r.ctx) // sends the wrong one
	var sid int64
	r.st.DB().QueryRow("SELECT session_id FROM items WHERE id = ?", wrong).Scan(&sid)
	r.st.Identify(r.ctx, sid, []model.Item{{Identity: model.Identity{Kind: model.KindMovie, Title: "right", TMDB: 9}, RuntimeMinutes: 100}}, []string{"trakt"}, time.Now())
	d.Run(r.ctx)
	if len(m.removed) != 1 || m.removed[0] != wrong {
		t.Errorf("removed %v", m.removed)
	}
	if len(m.pushed) != 2 {
		t.Errorf("pushed %v, want the wrong one then the right one", m.pushed)
	}
	var n int
	r.st.DB().QueryRow("SELECT COUNT(*) FROM items WHERE id = ?", wrong).Scan(&n)
	if n != 0 {
		t.Error("replaced item kept after its removal")
	}
}

func TestWatchedTimesSpread(t *testing.T) {
	end := t0.Add(2 * time.Hour)
	w := store.Watch{Session: &model.Session{StartedAt: t0, EndedAt: &end},
		Items: []model.Item{{ID: 1}, {ID: 2}}}
	got := WatchedTimes(w)
	if got[1] != t0.Add(time.Hour).UnixMilli() || got[2] != end.UnixMilli() {
		t.Errorf("%v", got)
	}
}
