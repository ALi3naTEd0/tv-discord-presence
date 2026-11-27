// Package androidtv listens to an Android TV / Google TV box over ADB: the
// app in front and the media session it publishes (state, real position
// and, when the app provides it, title and subtitle), plus the decoder's
// format (codec, resolution) and the content's dynamic range.
package androidtv

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// KnownApps names the streaming apps by package.
var KnownApps = map[string]string{
	"com.netflix.ninja":                          "Netflix",
	"com.wbd.stream":                             "HBO Max",
	"com.hbo.hbonow":                             "HBO Max",
	"com.disney.disneyplus":                      "Disney+",
	"com.amazon.amazonvideo.livingroom":          "Prime Video",
	"com.apple.atve.androidtv.appletv":           "Apple TV",
	"com.univision.prendetv":                     "ViX",
	"com.tubitv":                                 "Tubi",
	"com.mercadolibre.mercadoplay":               "Mercado Play",
	"com.clarovideo.androidtv":                   "Claro Video",
	"com.crunchyroll.crunchyroid":                "Crunchyroll",
	"com.google.android.youtube.tv":              "YouTube",
	"com.plexapp.android":                        "Plex",
	"com.spotify.tv.android":                     "Spotify",
	"tv.mirada.iris.android.inspire.izzitv.byod": "izzi tv",
}

// systemPackages are launchers, settings and the assistant.
var systemPackages = []string{"com.android.", "com.google.android.tvlauncher", "com.google.android.apps.tv.launcherx",
	"com.google.android.katniss", "com.mitv.", "com.xiaomi.", "android"}

// Source watches one box.
type Source struct {
	Addr     string // host:port, port 5555 by default
	DeviceID int64
	// KeyPath is the adb private key; created (with .pub) if missing.
	// The first connection shows "Allow USB debugging?" on the TV.
	KeyPath string
	Poll    time.Duration // 0 = 5 s
	Log     *slog.Logger

	// formats runs every this many polls (decoder records, HDR); 0 = 6.
	FormatsEvery int
}

// Kind implements source.Source.
func (s *Source) Kind() string { return "androidtv" }

func (s *Source) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Source) addr() string {
	if _, _, err := net.SplitHostPort(s.Addr); err == nil {
		return s.Addr
	}
	return net.JoinHostPort(s.Addr, "5555")
}

// Run implements source.Source.
func (s *Source) Run(ctx context.Context, emit func(model.Observation)) error {
	key, err := LoadOrCreateKey(s.KeyPath)
	if err != nil {
		return err
	}
	poll := s.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	every := s.FormatsEvery
	if every <= 0 {
		every = 6
	}
	r := &reader{seenCodecs: map[string]bool{}}
	var conn *adbConn
	var retryAt time.Time
	tick := time.NewTicker(poll)
	defer tick.Stop()
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	for n := 0; ; n++ {
		now := time.Now()
		if conn == nil && !now.Before(retryAt) {
			c, err := dialADB(ctx, s.addr(), key, 60*time.Second)
			if err != nil {
				s.log().Debug("Android TV unavailable", "addr", s.addr(), "err", err)
				if errors.Is(err, ErrUnauthorized) {
					s.log().Warn("Android TV: allow USB debugging for this computer on the TV", "addr", s.addr())
				}
				retryAt = now.Add(30 * time.Second)
			} else {
				conn = c
				s.log().Info("connected to Android TV", "addr", s.addr())
			}
		}
		o := model.Observation{DeviceID: s.DeviceID, At: now, State: model.StateOff}
		if conn != nil {
			if n%every == 0 {
				r.formats(conn)
			}
			if err := r.read(conn, &o); err != nil {
				s.log().Debug("Android TV read failed", "err", err)
				conn.Close()
				conn, o = nil, model.Observation{DeviceID: s.DeviceID, At: now, State: model.StateOff}
			}
		}
		emit(o)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// reader keeps what it needs between polls.
type reader struct {
	seenCodecs map[string]bool
	codecsRead bool
	video      *model.Video // last decoder format for the app in front
	videoApp   string
	hdr        string // dynamic range of the content now playing
	hdrLast    float64
	res        *resEvent // picture size the driver decodes right now (adaptive streaming moves it)
	resLast    float64
	resApp     string // app the live size belongs to (the one playing when it was first applied)
}

const stateCmd = "dumpsys power | grep -m1 mWakefulness=; echo @@; " +
	"dumpsys activity activities | grep -m1 mResumedActivity; echo @@; " +
	"cat /proc/uptime; echo @@; dumpsys media_session"

func (r *reader) read(c *adbConn, o *model.Observation) error {
	out, err := c.Shell(stateCmd, 20*time.Second)
	if err != nil {
		return err
	}
	parseState(out, o)
	if o.App.ID != "" && r.videoApp == o.App.ID && r.video != nil {
		v := *r.video
		o.Video = &v
	}
	if o.State == model.StatePlaying && r.res != nil && r.resApp != "" && r.resApp != o.App.ID {
		r.res = nil // another app took over and has not logged a size of its own
	}
	if o.State == model.StatePlaying && r.res != nil {
		r.resApp = o.App.ID
		if o.Video == nil {
			o.Video = &model.Video{Codec: r.res.codec}
		}
		if o.Video.Codec == "" {
			o.Video.Codec = r.res.codec
		}
		o.Video.Width, o.Video.Height = r.res.width, r.res.height
	}
	if o.State == model.StatePlaying && r.hdr != "" {
		if o.Video == nil {
			o.Video = &model.Video{}
		}
		o.Video.HDR = r.hdr
	}
	return nil
}

func (r *reader) formats(c *adbConn) {
	if out, err := c.Shell("dumpsys media.metrics | grep 'mediacodec.height' | grep 'mime=video' | tail -15", 20*time.Second); err == nil {
		for _, d := range parseDecoders(out, r.seenCodecs, !r.codecsRead) {
			v := model.Video{Codec: d.codec, Width: d.width, Height: d.height}
			r.video, r.videoApp = &v, d.app
		}
		r.codecsRead = true
	}
	if out, err := c.Shell(`logcat -d -b kernel -v epoch | grep -E 'visible\(|video changed to' | tail -40`, 20*time.Second); err == nil {
		var evs []resEvent
		evs, r.resLast = parseRes(out, r.resLast)
		if len(evs) > 0 {
			e := evs[len(evs)-1]
			r.res, r.resApp = &e, ""
		}
	}
	if out, err := c.Shell(`logcat -d -b kernel -v epoch | grep 'DV: \[' | tail -20`, 20*time.Second); err == nil {
		var evs []hdrEvent
		evs, r.hdrLast = parseHDR(out, r.hdrLast)
		if len(evs) > 0 {
			r.hdr = evs[len(evs)-1].format
		}
	}
}

var (
	reResumed  = regexp.MustCompile(` ([\w.]+)/`)
	reState    = regexp.MustCompile(`state=PlaybackState \{state=(\d+)`)
	rePosition = regexp.MustCompile(`position=(-?\d+).*?speed=([\d.]+), updated=(\d+)`)
	reDesc     = regexp.MustCompile(`description=([^\n]*)`)
	reBlock    = regexp.MustCompile(`\n    (\S)`)
)

// parseState fills an observation from the combined state command's output.
func parseState(out string, o *model.Observation) {
	parts := strings.SplitN(out, "@@", 4)
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	o.State = model.StateOff
	if !strings.Contains(parts[0], "Awake") {
		return
	}
	o.State = model.StateIdle
	pkg := ""
	if m := reResumed.FindStringSubmatch(parts[1]); m != nil {
		pkg = m[1]
	}
	if pkg == "" || isSystem(pkg) {
		return
	}
	uptimeMS := 0.0
	if f := strings.Fields(parts[2]); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			uptimeMS = v * 1000 // = elapsedRealtime, the clock "updated" uses
		}
	}
	play, title, subtitle, pos := parseMediaSession(parts[3], pkg, uptimeMS)
	name := KnownApps[pkg]
	if name == "" {
		if play == "" {
			return // an unknown app with no media session is not a player
		}
		i := strings.LastIndex(pkg, ".")
		name = strings.ToUpper(pkg[i+1:i+2]) + pkg[i+2:]
	}
	o.App = model.App{ID: pkg, Name: name}
	switch play {
	case "playing":
		o.State = model.StatePlaying
	case "paused":
		o.State = model.StatePaused
	}
	// The title the app publishes is also the media id: a new title is
	// new content (see engine.Device.TitleMediaIDs).
	o.MediaID = title
	o.Hint = model.Hint{Title: title, Subtitle: subtitle}
	o.Position = pos
}

func isSystem(pkg string) bool {
	for _, p := range systemPackages {
		if strings.HasPrefix(pkg, p) {
			return true
		}
	}
	return false
}

// parseMediaSession finds the package's media session in `dumpsys
// media_session`: playing wins over paused or buffering.
func parseMediaSession(sessions, pkg string, uptimeMS float64) (play, title, subtitle string, pos *time.Duration) {
	// Sessions are blocks indented by four spaces.
	blocks := reBlock.ReplaceAllString(sessions, "\n\x00$1")
	for _, block := range strings.Split(blocks, "\n\x00") {
		if !strings.Contains(block, "package="+pkg) {
			continue
		}
		code := 0
		if m := reState.FindStringSubmatch(block); m != nil {
			code, _ = strconv.Atoi(m[1])
		}
		// 3 playing, 6 buffering, 2 paused
		if !(code == 3 || (play == "" && (code == 2 || code == 6))) {
			continue
		}
		play = "paused"
		if code == 3 || code == 6 {
			play = "playing"
		}
		pos = nil
		if m := rePosition.FindStringSubmatch(block); m != nil {
			p, _ := strconv.ParseFloat(m[1], 64)
			speed, _ := strconv.ParseFloat(m[2], 64)
			upd, _ := strconv.ParseFloat(m[3], 64)
			if p >= 0 {
				// sampled at `updated`: advance to now while playing
				if code == 3 && uptimeMS > upd {
					p += (uptimeMS - upd) * speed
				}
				d := time.Duration(p) * time.Millisecond
				pos = &d
			}
		}
		title, subtitle = "", ""
		if m := reDesc.FindStringSubmatch(block); m != nil {
			f := strings.Split(m[1], ",")
			if t := strings.TrimSpace(f[0]); t != "" && t != "null" {
				title = t
			}
			if len(f) > 1 {
				if t := strings.TrimSpace(f[1]); t != "" && t != "null" {
					subtitle = t
				}
			}
		}
	}
	return
}

var (
	mimeNames = map[string]string{"video/av01": "AV1", "video/hevc": "HEVC", "video/avc": "H.264",
		"video/x-vnd.on2.vp9": "VP9", "video/dolby-vision": "Dolby Vision", "video/mpeg2": "MPEG-2"}
	reCodecField = regexp.MustCompile(`android\.media\.mediacodec\.([\w-]+)=([^,)]+)`)
	reCodecApp   = regexp.MustCompile(`\((\S+), \d+, \d+\)`)
	reHDR        = regexp.MustCompile(`^\s*(\d+\.\d+).*DV: \[[^,]*,[^,]*,\s*\d+,\s*([A-Za-z0-9+]+)`)
)

type decoder struct {
	app           string
	codec         string
	width, height int
}

// parseDecoders reads new video decoder records from media.metrics (Android
// logs one when an app releases a decoder). The first read only marks what
// is already there.
func parseDecoders(out string, seen map[string]bool, first bool) []decoder {
	var res []decoder
	for _, line := range strings.Split(out, "\n") {
		key := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		f := map[string]string{}
		for _, m := range reCodecField.FindAllStringSubmatch(line, -1) {
			f[m[1]] = strings.TrimSpace(strings.TrimRight(m[2], " }"))
		}
		app := reCodecApp.FindStringSubmatch(line)
		if first || f["encoder"] == "1" || app == nil || f["height"] == "" {
			continue
		}
		mime := f["mime"]
		codec := mimeNames[mime]
		if codec == "" {
			codec = strings.ToUpper(mime[strings.LastIndex(mime, "/")+1:])
		}
		w, _ := strconv.Atoi(f["width"])
		h, _ := strconv.Atoi(f["height"])
		res = append(res, decoder{app: app[1], codec: codec, width: w, height: h})
	}
	return res
}

var (
	reResCodec   = regexp.MustCompile(`^\s*(\d+\.\d+).*\]\s+(\w+) video changed to`)
	reResSize    = regexp.MustCompile(`^\s*(\d+\.\d+).*visible\((\d+) x (\d+)\)`)
	kernelCodecs = map[string]string{"hevc": "HEVC", "h265": "HEVC", "av1": "AV1", "vp9": "VP9",
		"h264": "H.264", "avc": "H.264", "mpeg2": "MPEG-2", "dv": "Dolby Vision"}
)

type resEvent struct {
	at            float64
	codec         string // "" when the driver did not name it just before
	width, height int
}

// parseRes reads live picture sizes from the Amlogic video driver: it logs
// `Parse from ucode, visible(W x H)` when a stream starts and every time
// adaptive streaming switches quality (HBO goes 576p -> 1080p -> 4K in
// seconds), while media.metrics only has a record once the decoder is released.
func parseRes(out string, last float64) ([]resEvent, float64) {
	var res []resEvent
	codec, codecAt := "", 0.0
	for _, line := range strings.Split(out, "\n") {
		if m := reResCodec.FindStringSubmatch(line); m != nil {
			codecAt, _ = strconv.ParseFloat(m[1], 64)
			codec = kernelCodecs[strings.ToLower(m[2])]
			continue
		}
		m := reResSize.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		at, _ := strconv.ParseFloat(m[1], 64)
		w, _ := strconv.Atoi(m[2])
		h, _ := strconv.Atoi(m[3])
		if at <= last || w < 320 {
			continue
		}
		last = at
		e := resEvent{at: at, width: w, height: h}
		if at-codecAt < 15 {
			e.codec = codec
		}
		res = append(res, e)
	}
	return res, last
}

type hdrEvent struct {
	at     float64
	format string // SDR, HDR10, HLG, DV
}

// parseHDR reads the content's dynamic range from the Amlogic video driver
// (`DV: [..]` in the kernel log, once per video start): the source format,
// unlike the HDMI output, which the box keeps in HDR.
func parseHDR(out string, last float64) ([]hdrEvent, float64) {
	var res []hdrEvent
	for _, line := range strings.Split(out, "\n") {
		m := reHDR.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		at, _ := strconv.ParseFloat(m[1], 64)
		if at > last {
			last = at
			res = append(res, hdrEvent{at: at, format: strings.ToUpper(m[2])})
		}
	}
	return res, last
}
