package androidtv

import (
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// A box playing Netflix, with YouTube paused in the background: shaped
// like `dumpsys media_session` on Android TV 9.
const mediaSessions = `MEDIA SESSION SERVICE (dumpsys media_session)
  Sessions Stack - have 2 sessions:
    YouTube com.google.android.youtube.tv/YouTube (userId=0)
      ownerPid=2211, ownerUid=10061, userId=0
      package=com.google.android.youtube.tv
      active=true
      state=PlaybackState {state=2, position=50000, buffered position=0, speed=0.0, updated=4000000, actions=0, custom actions=[], active item id=-1, error=null}
      metadata:size=3, description=Some video, Some channel, null
    Netflix com.netflix.ninja/Netflix (userId=0)
      ownerPid=1234, ownerUid=10050, userId=0
      package=com.netflix.ninja
      active=true
      state=PlaybackState {state=3, position=1200000, buffered position=0, speed=1.0, updated=5000000, actions=1590, custom actions=[], active item id=-1, error=null}
      metadata:size=5, description=Ozark, Season 1: Episode 2, null
`

func stateOut(wake, resumed, uptime, sessions string) string {
	return wake + "\n@@\n" + resumed + "\n@@\n" + uptime + "\n@@\n" + sessions
}

const awake = "  mWakefulness=Awake"

func TestParseStatePlaying(t *testing.T) {
	var o model.Observation
	// uptime 5010 s: 10 s after Netflix sampled its position
	parseState(stateOut(awake, "    mResumedActivity: ActivityRecord{abc u0 com.netflix.ninja/.MainActivity t12}", "5010.00 9000.00", mediaSessions), &o)
	if o.State != model.StatePlaying || o.App.ID != "com.netflix.ninja" || o.App.Name != "Netflix" {
		t.Fatalf("observation %+v", o)
	}
	if o.Hint.Title != "Ozark" || o.Hint.Subtitle != "Season 1: Episode 2" || o.MediaID != "Ozark" {
		t.Errorf("hint %+v media %q", o.Hint, o.MediaID)
	}
	if o.Position == nil || *o.Position != 1210*time.Second {
		t.Errorf("position %v, want 1210 s (advanced to now)", o.Position)
	}
}

func TestParseStatePausedDoesNotAdvance(t *testing.T) {
	var o model.Observation
	parseState(stateOut(awake, " mResumedActivity: ActivityRecord{x u0 com.google.android.youtube.tv/.Main t3}", "5010.0 1", mediaSessions), &o)
	if o.State != model.StatePaused || o.Position == nil || *o.Position != 50*time.Second || o.Hint.Subtitle != "Some channel" {
		t.Errorf("observation %+v pos %v", o, o.Position)
	}
}

func TestParseStateOffAndSystem(t *testing.T) {
	var o model.Observation
	parseState(stateOut("  mWakefulness=Asleep", "", "1 1", ""), &o)
	if o.State != model.StateOff {
		t.Errorf("asleep: %+v", o)
	}
	o = model.Observation{}
	parseState(stateOut(awake, " mResumedActivity: ActivityRecord{x u0 com.google.android.tvlauncher/.Main t1}", "1 1", mediaSessions), &o)
	if o.State != model.StateIdle || o.App.ID != "" {
		t.Errorf("launcher: %+v", o)
	}
	// An unknown app with no media session is not a player.
	o = model.Observation{}
	parseState(stateOut(awake, " mResumedActivity: ActivityRecord{x u0 com.example.weather/.Main t1}", "1 1", mediaSessions), &o)
	if o.App.ID != "" {
		t.Errorf("unknown app reported: %+v", o)
	}
	// A known app open in its menu, no session: idle in that app.
	o = model.Observation{}
	parseState(stateOut(awake, " mResumedActivity: ActivityRecord{x u0 com.disney.disneyplus/.Main t1}", "1 1", mediaSessions), &o)
	if o.State != model.StateIdle || o.App.Name != "Disney+" || o.MediaID != "" {
		t.Errorf("app menu: %+v", o)
	}
}

func TestParseStateUnknownPlayer(t *testing.T) {
	s := strings.ReplaceAll(mediaSessions, "com.netflix.ninja", "org.some.player")
	var o model.Observation
	parseState(stateOut(awake, " mResumedActivity: ActivityRecord{x u0 org.some.player/.Main t1}", "5000 1", s), &o)
	if o.App.Name != "Player" || o.State != model.StatePlaying {
		t.Errorf("unknown player: %+v", o)
	}
}

func TestParseDecoders(t *testing.T) {
	out := `   12: { (com.netflix.ninja, 1234, 10050) android.media.mediacodec.codec=c2.amlogic.hevc.decoder,android.media.mediacodec.mime=video/hevc,android.media.mediacodec.width=3840,android.media.mediacodec.height=2160,android.media.mediacodec.encoder=0 }
   13: { (com.example.cam, 1, 1) android.media.mediacodec.mime=video/avc,android.media.mediacodec.width=1280,android.media.mediacodec.height=720,android.media.mediacodec.encoder=1 }`
	seen := map[string]bool{}
	if got := parseDecoders(out, seen, true); len(got) != 0 {
		t.Errorf("first read reported %+v; it should only mark", got)
	}
	out += "\n   14: { (com.wbd.stream, 9, 9) android.media.mediacodec.mime=video/av01,android.media.mediacodec.width=1920,android.media.mediacodec.height=1080 }"
	got := parseDecoders(out, seen, false)
	if len(got) != 1 || got[0].app != "com.wbd.stream" || got[0].codec != "AV1" || got[0].width != 1920 || got[0].height != 1080 {
		t.Errorf("decoders %+v", got)
	}
}

func TestParseHDR(t *testing.T) {
	out := `1764100000.123 kernel: [  10.1] DV: [1, 2, 10, HDR10, 0]
1764100500.500 kernel: [  20.2] DV: [1, 2, 8, sdr, 0]`
	evs, last := parseHDR(out, 0)
	if len(evs) != 2 || evs[1].format != "SDR" || last != 1764100500.5 {
		t.Fatalf("events %+v last %v", evs, last)
	}
	if evs, _ := parseHDR(out, last); len(evs) != 0 {
		t.Errorf("old events reported again: %+v", evs)
	}
}
