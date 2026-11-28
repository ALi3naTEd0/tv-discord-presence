package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// Session is one viewing as the dashboard shows it.
type Session struct {
	ID          int64      `json:"id"`
	Device      Device     `json:"device"`
	App         App        `json:"app"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	Open        bool       `json:"open"`
	Hidden      string     `json:"hidden,omitempty"`
	PlayedSecs  int        `json:"played_secs"`
	PositionSec *int       `json:"position_secs,omitempty"`      // furthest point reached
	LiveSec     *int       `json:"live_position_secs,omitempty"` // last sample
	Hint        *Hint      `json:"hint,omitempty"`               // what the device published
	Quality     *Quality   `json:"quality,omitempty"`
	Items       []Item     `json:"items"`
	// Progress is 0-1 of the current item's runtime, from the live position
	// while open, else the furthest one. Omitted without a runtime.
	Progress *float64 `json:"progress,omitempty"`
	// EndsAt is when what plays now will finish by the server's clock: now
	// plus what is left. Only for an open session with a known runtime.
	EndsAt       *time.Time `json:"ends_at,omitempty"`
	AutoNextFrom *int64     `json:"auto_next_from,omitempty"`
}

// App is the app that played it.
type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Hint is what the device itself said was playing.
type Hint struct {
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Season   int    `json:"season,omitempty"`
	Episode  int    `json:"episode,omitempty"`
}

// Quality is what the device actually decoded, in the words a person uses.
type Quality struct {
	Resolution string `json:"resolution,omitempty"` // 4K, 1080p, 720p, 576p
	UHD        bool   `json:"uhd,omitempty"`        // 4K: shown as "4K UHD"
	Codec      string `json:"codec,omitempty"`      // HEVC, AV1, H.264, VP9
	HDR        string `json:"hdr,omitempty"`        // HDR10, HLG, Dolby Vision; "" for SDR (not shown)
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Label      string `json:"label"` // "4K UHD · HEVC · Dolby Vision"
}

// Item is what the session was.
type Item struct {
	ID           int64               `json:"id"`
	Kind         string              `json:"kind"`
	Title        string              `json:"title"`
	Year         int                 `json:"year,omitempty"`
	Season       int                 `json:"season,omitempty"`
	Episode      int                 `json:"episode,omitempty"`
	EpisodeTitle string              `json:"episode_title,omitempty"`
	Poster       string              `json:"poster,omitempty"`
	RuntimeMin   int                 `json:"runtime_min,omitempty"`
	RuntimeEst   bool                `json:"runtime_estimated,omitempty"`
	Guessed      bool                `json:"guessed,omitempty"` // a guess from your progress: confirm to send
	IDs          IDs                 `json:"ids"`
	Links        []Link              `json:"links"`
	Deliveries   map[string]Delivery `json:"deliveries"`
}

// IDs are the item's external IDs (of the show, for episodes).
type IDs struct {
	TMDB int    `json:"tmdb,omitempty"`
	TVDB int    `json:"tvdb,omitempty"`
	IMDb string `json:"imdb,omitempty"`
}

// Link is a small icon link to a site that knows the title. Couchlog is not
// a database; it points to the ones people use.
type Link struct {
	Site string `json:"site"` // imdb | tmdb | trakt
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Delivery is the item's status with one destination.
type Delivery struct {
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (a *API) view(ctx context.Context, s *model.Session, devs map[int64]store.Device) (Session, error) {
	d := devs[s.DeviceID]
	v := Session{
		ID: s.ID, Device: Device{ID: d.ID, Kind: d.Kind, Name: d.Name}, App: App{ID: s.App.ID, Name: s.App.Name},
		StartedAt: s.StartedAt, EndedAt: s.EndedAt, LastSeenAt: s.LastSeenAt, Open: s.Open, Hidden: string(s.Hidden),
		PlayedSecs: int(s.Played.Seconds()), PositionSec: secs(s.Position), LiveSec: secs(s.LivePosition),
		Quality: QualityOf(s.Video), Items: []Item{}, AutoNextFrom: s.AutoNextFrom,
	}
	if h := s.Hint; h != (model.Hint{}) {
		v.Hint = &Hint{Title: h.Title, Subtitle: h.Subtitle, Season: h.Season, Episode: h.Episode}
	}
	items, err := a.Store.Items(ctx, s.ID)
	if err != nil {
		return v, err
	}
	for _, it := range items {
		dels, err := a.Store.Deliveries(ctx, it.ID)
		if err != nil {
			return v, err
		}
		iv := Item{ID: it.ID, Kind: string(it.Kind), Title: it.Title, Year: it.Year, Season: it.Season,
			Episode: it.Episode, EpisodeTitle: it.EpisodeTitle, Poster: it.Poster, RuntimeMin: it.RuntimeMinutes,
			RuntimeEst: it.RuntimeEstimated, Guessed: it.Guessed, IDs: IDs{TMDB: it.TMDB, TVDB: it.TVDB, IMDb: it.IMDb},
			Links: LinksOf(it.Identity), Deliveries: map[string]Delivery{}}
		for _, dl := range dels {
			iv.Deliveries[dl.Sink] = Delivery{Status: string(dl.Status), Error: dl.Error, UpdatedAt: dl.UpdatedAt}
		}
		v.Items = append(v.Items, iv)
	}
	// Progress and the end time follow the last item: on autoplay splits and
	// multi-episode sessions, it is the one playing.
	if n := len(v.Items); n > 0 && v.Items[n-1].RuntimeMin > 0 {
		total := time.Duration(v.Items[n-1].RuntimeMin) * time.Minute
		pos := s.Position
		if s.Open && s.LivePosition != nil {
			pos = s.LivePosition
		}
		if pos != nil {
			p := min(1, max(0, float64(*pos)/float64(total)))
			v.Progress = &p
			if s.Open && *pos < total {
				end := a.now().Add(total - *pos).Truncate(time.Second)
				v.EndsAt = &end
			}
		}
	}
	return v, nil
}

func secs(d *time.Duration) *int {
	if d == nil {
		return nil
	}
	n := int(d.Seconds())
	return &n
}

// QualityOf turns a decoded format into plain words. Resolution goes by
// width or height: scope films are 3840×1606 (still 4K) or 1920×800 (still
// 1080p), and 4:3 masters 1440×1080 (still 1080p). SDR is the norm, so it is
// left out; HDR formats are named.
func QualityOf(v *model.Video) *Quality {
	if v == nil || (v.Width == 0 && v.Codec == "" && v.HDR == "") {
		return nil
	}
	q := &Quality{Codec: v.Codec, Width: v.Width, Height: v.Height}
	switch w, h := v.Width, v.Height; {
	case w == 0 && h == 0:
	case w >= 3000 || h >= 2000:
		q.Resolution, q.UHD = "4K", true
	case w >= 1800 || h >= 1000:
		q.Resolution = "1080p"
	case w >= 1200 || h >= 700:
		q.Resolution = "720p"
	default:
		q.Resolution = fmt.Sprintf("%dp", h)
	}
	switch strings.ToUpper(v.HDR) {
	case "", "SDR":
	case "DV", "DOLBY VISION":
		q.HDR = "Dolby Vision"
	case "HDR10+", "HDR10PLUS":
		q.HDR = "HDR10+"
	default:
		q.HDR = strings.ToUpper(v.HDR)
	}
	if q.Codec == "Dolby Vision" { // the decoder's name for it; the range says it already
		q.Codec = ""
		if q.HDR == "" {
			q.HDR = "Dolby Vision"
		}
	}
	var parts []string
	if q.Resolution != "" {
		r := q.Resolution
		if q.UHD {
			r += " UHD"
		}
		parts = append(parts, r)
	}
	for _, p := range []string{q.Codec, q.HDR} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	q.Label = strings.Join(parts, " · ")
	return q
}

// LinksOf builds links to IMDb, TMDB and Trakt from the IDs it has.
func LinksOf(id model.Identity) []Link {
	links := []Link{}
	ep := id.Kind == model.KindEpisode && id.Season > 0 && id.Episode > 0
	if id.IMDb != "" {
		// for episodes it is the show's IMDb id: the episode's own is not kept
		links = append(links, Link{Site: "imdb", Name: "IMDb", URL: "https://www.imdb.com/title/" + id.IMDb + "/"})
	}
	if id.TMDB != 0 {
		u := fmt.Sprintf("https://www.themoviedb.org/movie/%d", id.TMDB)
		if id.Kind == model.KindEpisode {
			u = fmt.Sprintf("https://www.themoviedb.org/tv/%d", id.TMDB)
			if ep {
				u += fmt.Sprintf("/season/%d/episode/%d", id.Season, id.Episode)
			}
		}
		links = append(links, Link{Site: "tmdb", Name: "TMDB", URL: u})
		kind := "movie"
		if id.Kind == model.KindEpisode {
			kind = "show"
		}
		links = append(links, Link{Site: "trakt", Name: "Trakt",
			URL: fmt.Sprintf("https://trakt.tv/search/tmdb/%d?id_type=%s", id.TMDB, kind)})
	}
	return links
}
