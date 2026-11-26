package engine

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

var t0 = time.Date(2026, 11, 25, 21, 0, 0, 0, time.UTC)

const (
	netflix = "netflix"
	youtube = "youtube.leanback.v4"
	twitch  = "tv.twitch.tv.starshot.lg"
)

type rig struct {
	t      *testing.T
	ctx    context.Context
	st     *store.Store
	dev    Device
	tr     *Tracker
	events []Event
}

func newRig(t *testing.T, titleIDs bool) *rig {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "couchlog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.EnsureDevice(ctx, store.LocalUserID, "webos", "LG TV", "")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, ctx: ctx, st: st, dev: Device{ID: id, TitleMediaIDs: titleIDs}}
	r.restart(t0)
	return r
}

// restart builds a new tracker on the same database, as after a crash.
func (r *rig) restart(now time.Time) {
	r.t.Helper()
	tr, err := NewTracker(r.ctx, r.st, r.dev, Config{}, now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		r.t.Fatal(err)
	}
	tr.OnEvent = func(e Event) { r.events = append(r.events, e) }
	r.tr = tr
}

type obsOpt func(*model.Observation)

func media(id string) obsOpt { return func(o *model.Observation) { o.MediaID = id } }
func hint(title string) obsOpt {
	return func(o *model.Observation) { o.Hint = model.Hint{Title: title, Season: 1, Episode: 2} }
}
func pos(d time.Duration) obsOpt { return func(o *model.Observation) { o.Position = &d } }
func video(w int, hdr string) obsOpt {
	return func(o *model.Observation) {
		o.Video = &model.Video{Codec: "HEVC", Width: w, Height: w * 9 / 16, HDR: hdr}
	}
}

// at feeds one observation at t0+sec.
func (r *rig) at(sec int, state model.PlayState, app string, opts ...obsOpt) *model.Session {
	r.t.Helper()
	o := model.Observation{DeviceID: r.dev.ID, At: t0.Add(time.Duration(sec) * time.Second), State: state}
	if app != "" {
		o.App = model.App{ID: app, Name: app}
	}
	for _, f := range opts {
		f(&o)
	}
	s, err := r.tr.Observe(r.ctx, o)
	if err != nil {
		r.t.Fatalf("observe at %ds: %v", sec, err)
	}
	return s
}

// play feeds "playing" every 5 s from..to (inclusive), like a 5 s poll.
func (r *rig) play(from, to int, app string, opts ...obsOpt) *model.Session {
	r.t.Helper()
	var s *model.Session
	for sec := from; sec <= to; sec += 5 {
		s = r.at(sec, model.StatePlaying, app, opts...)
	}
	return s
}

func (r *rig) sessions() []*model.Session {
	r.t.Helper()
	rows, err := r.st.DB().QueryContext(r.ctx, "SELECT id FROM sessions ORDER BY id")
	if err != nil {
		r.t.Fatal(err)
	}
	var ids []int64 // read all ids first: the store has a single connection
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*model.Session
	for _, id := range ids {
		s, err := r.st.Session(r.ctx, id)
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (r *rig) wantSessions(n int) []*model.Session {
	r.t.Helper()
	ss := r.sessions()
	if len(ss) != n {
		r.t.Fatalf("%d sessions, want %d", len(ss), n)
	}
	return ss
}

func TestCountsOnlyPlayingTime(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 300, netflix, media("m1"))
	for sec := 305; sec <= 600; sec += 5 { // paused 5 min: not counted
		r.at(sec, model.StatePaused, netflix, media("m1"))
	}
	r.play(605, 900, netflix, media("m1"))
	s := r.wantSessions(1)[0]
	if want := 595 * time.Second; s.Played != want { // 300 + 295: the first sample after a pause adds nothing
		t.Errorf("played %v, want %v", s.Played, want)
	}
	if !s.Open || !s.StartedAt.Equal(t0) {
		t.Errorf("open=%v started=%v", s.Open, s.StartedAt)
	}
}

func TestGapAfterSuspendNotCounted(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 200, netflix, media("m1"))
	r.play(260, 400, netflix, media("m1")) // 60 s without samples (network drop)
	s := r.wantSessions(1)[0]
	if want := 340 * time.Second; s.Played != want {
		t.Errorf("played %v, want %v", s.Played, want)
	}
}

func TestShortUnidentifiedSessionDropped(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 60, netflix, media("trailer"))
	r.at(65, model.StateIdle, "")
	r.at(130, model.StateIdle, "") // grace over
	r.wantSessions(0)
	if len(r.events) != 2 || r.events[1].Outcome != store.Dropped {
		t.Errorf("events %+v, want opened then dropped", r.events)
	}
}

func TestCloseGrace(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 300, netflix, media("m1"))
	r.at(305, model.StateOff, "")
	if s := r.at(365, model.StateOff, ""); s == nil {
		t.Fatal("closed at exactly the grace; it must wait longer than close_grace")
	}
	if s := r.at(366, model.StateOff, ""); s != nil {
		t.Fatal("still open after the grace")
	}
	s := r.wantSessions(1)[0]
	if s.Open || s.EndedAt == nil || !s.EndedAt.Equal(t0.Add(300*time.Second)) {
		t.Errorf("ended at %v, want the last time it was seen in the app", s.EndedAt)
	}
}

func TestBriefAppSwitchOutsideRecordedAppsKeepsSession(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 300, netflix, media("m1"))
	r.at(305, model.StateIdle, "") // home screen for 20 s
	r.at(325, model.StateIdle, "")
	r.play(330, 600, netflix, media("m1"))
	r.wantSessions(1)
}

func TestSwitchingAppsSplits(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 300, netflix, media("m1"))
	r.play(305, 700, "hbo", media("h1"))
	ss := r.wantSessions(2)
	if ss[0].Open || ss[0].App.ID != netflix || !ss[1].Open || ss[1].App.ID != "hbo" {
		t.Errorf("sessions %+v", ss)
	}
}

func TestPauseTimeout(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 300, netflix, media("m1"))
	if s := r.at(300+30*60, model.StatePaused, netflix, media("m1")); s == nil {
		t.Fatal("closed at exactly the pause timeout")
	}
	if s := r.at(301+30*60, model.StatePaused, netflix, media("m1")); s != nil {
		t.Fatal("still open after 30 min paused")
	}
	if s := r.wantSessions(1)[0]; s.Open {
		t.Error("session still open")
	}
}

func TestWebOSMediaChangeSplitsOnlyAfterMenu(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 300, netflix, media("ep1"))
	r.play(305, 600, netflix, media("ep2")) // autoplay / credits: same session
	r.wantSessions(1)
	r.at(605, model.StatePaused, netflix) // back in the app's menu: no media id
	r.play(610, 900, netflix, media("ep5"))
	ss := r.wantSessions(2)
	if ss[0].MediaID != "ep2" || ss[1].MediaID != "ep5" {
		t.Errorf("media ids %q, %q", ss[0].MediaID, ss[1].MediaID)
	}
}

func TestTitleMediaIDsSplitAndReopen(t *testing.T) {
	r := newRig(t, true)
	r.play(0, 600, netflix, media("Dark"), hint("Dark"))
	r.play(605, 1200, netflix, media("Ozark")) // new title: new content right away
	ss := r.wantSessions(2)
	if ss[0].Open || ss[0].Hint.Title != "Dark" || ss[0].Hint.Episode != 2 {
		t.Errorf("first session %+v", ss[0])
	}
	r.play(1205, 1800, "hbo", media("Succession"))
	// Back to Dark two hours later: the Dark session continues.
	s := r.play(2*3600, 2*3600+300, netflix, media("Dark"))
	r.wantSessions(3)
	if s.ID != ss[0].ID || !s.Open {
		t.Errorf("Dark came back as session %d, want %d reopened", s.ID, ss[0].ID)
	}
	if r.events[len(r.events)-1].Kind != Resumed {
		t.Errorf("last event %+v, want resumed", r.events[len(r.events)-1])
	}
}

func TestReopenSkipsIgnoredAndOld(t *testing.T) {
	r := newRig(t, true)
	r.play(0, 600, netflix, media("Dark"))
	r.play(605, 1200, netflix, media("Ozark"))
	first := r.sessions()[0]
	if _, err := r.st.DB().Exec("UPDATE sessions SET hidden = 'ignored' WHERE id = ?", first.ID); err != nil {
		t.Fatal(err)
	}
	r.play(1205, 1500, netflix, media("Dark"))
	r.wantSessions(3)
	ozark := r.sessions()[1]
	if s := r.play(1505, 1800, netflix, media("Ozark")); s.ID != ozark.ID { // not ignored: continues
		t.Errorf("Ozark came back as session %d, want %d", s.ID, ozark.ID)
	}
	r.play(1805, 2000, netflix, media("Mindhunter"))
	r.play(1800+7*3600, 1800+7*3600+300, netflix, media("Ozark")) // 7 h later: new session
	r.wantSessions(5)
}

func TestYouTubeUnderAppMinimumHiddenAsShort(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 600, youtube, media("v1")) // 10 min < 20 min
	r.play(605, 900, netflix, media("m1"))
	s := r.sessions()[0]
	if s.Open || s.Hidden != model.Short {
		t.Errorf("youtube session open=%v hidden=%q, want closed short", s.Open, s.Hidden)
	}
}

func TestIdentifiedShortSessionKept(t *testing.T) {
	r := newRig(t, false)
	s := r.play(0, 60, netflix, media("m1"))
	if _, err := r.st.DB().Exec("INSERT INTO items(session_id, kind, title) VALUES (?, 'movie', 'Short film')", s.ID); err != nil {
		t.Fatal(err)
	}
	r.play(65, 300, "hbo", media("h1"))
	if got := r.sessions()[0]; got.ID != s.ID || got.Hidden != model.Shown {
		t.Errorf("identified short session not kept as shown: %+v", got)
	}
}

func TestExcludedAppNeverRecords(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 900, twitch, media("stream"))
	r.wantSessions(0)
}

func TestNotPlayingDoesNotOpen(t *testing.T) {
	r := newRig(t, false)
	for sec := 0; sec <= 600; sec += 5 {
		r.at(sec, model.StateIdle, netflix) // browsing the catalogue
	}
	r.wantSessions(0)
}

func TestRestartResumesRecentSession(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 600, netflix, media("m1"))
	r.restart(t0.Add(11 * time.Minute)) // seen 1 min ago
	if got := r.wantSessions(1)[0]; got.Played != 600*time.Second {
		t.Fatalf("played %v before resuming", got.Played)
	}
	s := r.at(660, model.StatePlaying, netflix, media("m1"))
	if s.Played != 600*time.Second {
		t.Fatalf("first sample after restart added time: %v", s.Played)
	}
	s = r.play(665, 900, netflix, media("m1"))
	ss := r.wantSessions(1)
	if s.ID != ss[0].ID || ss[0].Played != 600*time.Second+240*time.Second {
		t.Errorf("played %v after resume", ss[0].Played)
	}
}

func TestRestartClosesStaleSession(t *testing.T) {
	r := newRig(t, false)
	r.play(0, 600, netflix, media("m1"))
	r.restart(t0.Add(30 * time.Minute)) // 20 min later: too old
	if s := r.wantSessions(1)[0]; s.Open {
		t.Fatal("stale session kept open after restart")
	}
	r.play(1800, 2100, netflix, media("m1"))
	r.wantSessions(2)
}

func TestDashboardClosedSessionMeanwhile(t *testing.T) {
	r := newRig(t, false)
	s := r.play(0, 600, netflix, media("ep1"))
	// The dashboard's "next episode" closes it; the tracker follows the database.
	if _, err := r.st.CloseSession(r.ctx, s.ID, 0, 0); err != nil {
		t.Fatal(err)
	}
	next := r.play(605, 900, netflix, media("ep1"))
	if next.ID == s.ID {
		t.Fatal("kept writing into the session the dashboard closed")
	}
	if got, _ := r.st.Session(r.ctx, s.ID); got.Played != 600*time.Second {
		t.Errorf("closed session changed: played %v", got.Played)
	}
}

func TestPositionVideoAndHDR(t *testing.T) {
	r := newRig(t, true)
	r.play(0, 60, netflix, media("Dune"), video(1280, "SDR"), pos(10*time.Second)) // trailer-ish start
	r.play(65, 900, netflix, media("Dune"), video(3840, "DV"), pos(900*time.Second))
	r.at(905, model.StatePlaying, netflix, media("Dune"), video(1920, "DV"), pos(5*time.Second)) // ABR drop, back to start
	s := r.wantSessions(1)[0]
	if s.Video == nil || s.Video.Width != 3840 || s.Video.HDR != "DV" {
		t.Errorf("video %+v, want the widest (3840) and the longest range (DV)", s.Video)
	}
	if *s.Position != 900*time.Second || *s.LivePosition != 5*time.Second {
		t.Errorf("position %v live %v", *s.Position, *s.LivePosition)
	}
	if s.HDRTime["SDR"] != 60*time.Second || s.HDRTime["DV"] != 840*time.Second+5*time.Second {
		t.Errorf("hdr time %v", s.HDRTime)
	}
}

func TestHintKeepsFirst(t *testing.T) {
	r := newRig(t, true)
	r.play(0, 300, netflix, media("Dark"), hint("Dark"))
	s := r.play(305, 400, netflix, media("Dark"), hint("Next up: Ozark"))
	if s.Hint.Title != "Dark" {
		t.Errorf("hint %q, want the first one kept", s.Hint.Title)
	}
}
