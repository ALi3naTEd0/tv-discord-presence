package presence

import (
	"fmt"
	"strings"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Activity is a Discord status, as Discord's local API takes it.
type Activity struct {
	Type              int         `json:"type"`                          // 3 = Watching
	Name              string      `json:"name,omitempty"`                // the app, in "Watching <name>"
	StatusDisplayType int         `json:"status_display_type,omitempty"` // 2 = show the details line in the member list
	Details           string      `json:"details,omitempty"`
	State             string      `json:"state,omitempty"`
	Timestamps        *Timestamps `json:"timestamps,omitempty"`
	Assets            *Assets     `json:"assets,omitempty"`
	Buttons           []Button    `json:"buttons,omitempty"` // up to two links others can open
}

// Button is a link under the status.
type Button struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Timestamps make Discord show elapsed time, or a progress bar with End.
type Timestamps struct {
	Start int64 `json:"start,omitempty"` // Unix seconds
	End   int64 `json:"end,omitempty"`
}

// Assets are the images: the poster large, the app's logo small.
type Assets struct {
	LargeImage string `json:"large_image,omitempty"`
	LargeText  string `json:"large_text,omitempty"`
	SmallImage string `json:"small_image,omitempty"`
	SmallText  string `json:"small_text,omitempty"`
}

const (
	typeWatching    = 3
	displayName     = 0
	displayDetails  = 2
	maxDiscordField = 128
)

// logoDomains give each app's logo (through Google's favicon service).
var logoDomains = map[string]string{
	"netflix": "www.netflix.com", "com.netflix.ninja": "www.netflix.com",
	"com.wbd.stream": "www.hbomax.com", "com.hbo.hbomax": "www.hbomax.com", "com.hbo.hbonow": "www.hbomax.com",
	"com.disney.disneyplus-prod": "www.disneyplus.com", "com.disney.disneyplus": "www.disneyplus.com",
	"hulu": "www.hulu.com", "amazon": "www.primevideo.com", "com.amazon.amazonvideo.livingroom": "www.primevideo.com",
	"com.apple.appletv": "tv.apple.com", "com.apple.atve.androidtv.appletv": "tv.apple.com",
	"youtube.leanback.v4": "www.youtube.com", "com.google.android.youtube.tv": "www.youtube.com",
	"vix": "vix.com", "com.univision.prendetv": "vix.com",
	"com.clarovideov9": "www.clarovideo.com", "com.clarovideo.androidtv": "www.clarovideo.com",
	"com.mercadolibre.mercadoplay": "play.mercadolibre.com.mx",
	"com.tubitv.ott.tubi":          "tubitv.com", "com.tubitv": "tubitv.com",
	"com.crunchyroll.crunchyroid": "www.crunchyroll.com",
	"com.izzi.tv":                 "www.izzi.mx", "tv.mirada.iris.android.inspire.izzitv.byod": "www.izzi.mx",
	"plex": "www.plex.tv", "com.plexapp.android": "www.plex.tv",
}

// Logo is an app's logo URL, or "" for apps without a known site.
func Logo(appID string) string {
	d := logoDomains[appID]
	if d == "" {
		return ""
	}
	return "https://t0.gstatic.com/faviconV2?client=SOCIAL&type=FAVICON&fallback_opts=TYPE,SIZE,URL&url=https://" + d + "&size=256"
}

// Now is what plays on one device right now.
type Now struct {
	App      model.App
	State    model.PlayState // playing or paused
	Item     *model.Item     // nil while unidentified
	Hint     model.Hint      // what the device published, if anything
	Position *time.Duration  // real position, when the device knows it
	Played   time.Duration   // time played in this session
	Device   string          // where (Plex players), "" for the TV
}

func clip(s string) string {
	if len([]rune(s)) <= maxDiscordField {
		return s
	}
	return string([]rune(s)[:maxDiscordField])
}

// Build turns what plays into a Discord status, the way tvpresence shows it:
// identified (title, episode line, poster, progress bar with a real
// position and runtime), published by the app (title and subtitle), or just
// the app. nil when nothing plays.
func Build(n *Now, now time.Time) *Activity {
	if n == nil || (n.State != model.StatePlaying && n.State != model.StatePaused) || n.App.ID == "" {
		return nil
	}
	paused := n.State == model.StatePaused
	logo := Logo(n.App.ID)
	// Elapsed: from the real position when known, else the played time.
	var start time.Time
	switch {
	case n.Position != nil:
		start = now.Add(-*n.Position)
	case n.Played > 0:
		start = now.Add(-n.Played)
	default:
		start = now
	}
	ts := &Timestamps{Start: start.Unix()}
	if paused {
		ts = nil
	}
	a := &Activity{Type: typeWatching, Name: n.App.Name, Timestamps: ts}

	if it := n.Item; it != nil {
		title := it.Title
		line := "Movie"
		if it.Kind == model.KindMovie && it.Year != 0 {
			title += fmt.Sprintf(" (%d)", it.Year)
		}
		if it.Kind == model.KindEpisode {
			line = fmt.Sprintf("S%d · E%d", it.Season, it.Episode)
			if it.EpisodeTitle != "" {
				line += " — " + it.EpisodeTitle
			}
		}
		if paused {
			line = "Paused · " + line
		}
		a.StatusDisplayType, a.Details, a.State = displayDetails, clip(title), clip(line)
		a.Assets = &Assets{LargeImage: logo, LargeText: clip(title)}
		if it.Poster != "" {
			where := n.App.Name + " on TV"
			if n.Device != "" {
				where = n.App.Name + " · " + n.Device
			}
			a.Assets = &Assets{LargeImage: it.Poster, LargeText: clip(title), SmallImage: logo, SmallText: clip(where)}
		}
		if it.IMDb != "" {
			a.Buttons = []Button{{Label: "IMDb", URL: "https://www.imdb.com/title/" + it.IMDb + "/"}}
		}
		// A progress bar only with a real position and a runtime that is not a guess.
		if ts != nil && n.Position != nil && it.RuntimeMinutes > 0 && !it.RuntimeEstimated {
			ts.End = start.Add(time.Duration(it.RuntimeMinutes) * time.Minute).Unix()
		}
		return a
	}
	if t := strings.TrimSpace(n.Hint.Title); t != "" {
		state := n.Hint.Subtitle
		switch {
		case state != "" && paused:
			state = "Paused · " + state
		case state == "" && paused:
			state = "Paused"
		case state == "":
			state = n.App.Name + " on TV"
		}
		a.StatusDisplayType, a.Details, a.State = displayDetails, clip(t), clip(state)
		a.Assets = &Assets{LargeImage: logo, LargeText: n.App.Name}
		return a
	}
	state := "Playing on TV"
	if paused {
		state = "Paused on TV"
	}
	a.StatusDisplayType, a.Details, a.State = displayName, "Watching "+n.App.Name, state
	a.Assets = &Assets{LargeImage: logo, LargeText: n.App.Name}
	return a
}

// Line is a one-line summary, for logs and the dry run.
func (a *Activity) Line() string {
	if a == nil {
		return "(cleared)"
	}
	return strings.TrimRight(a.Name+" — "+a.Details+" — "+a.State, " —")
}
