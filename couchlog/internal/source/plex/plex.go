// Package plex reads a Plex Media Server: its watch history (already
// identified: Plex knows the IMDb/TMDB/TVDB ids of what it plays) and what
// is playing right now, for the presence and the dashboard's "now" card.
//
// Plex is not polled into sessions like a TV: Plex itself records each
// viewing, so Couchlog imports those (source.Importer).
package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
)

// Client talks to one Plex server as one account.
type Client struct {
	URL       string // e.g. http://127.0.0.1:32400
	Token     string // X-Plex-Token
	AccountID int    // whose history; 1 = the server owner
	User      string // optional account name, for "now playing" on shared servers
	DeviceID  int64  // Couchlog's device row for this server
	HTTP      *http.Client

	// DBPath is Plex's library database (DefaultDBPath on the Plex machine).
	// When readable, plays come from it: it keeps the IDs of deleted items.
	DBPath      string
	ProviderURL string // "" = Plex's metadata provider (tests override)

	provMu sync.Mutex
	prov   map[string]provided
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("plex: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plex: HTTP %d on %s", resp.StatusCode, strings.SplitN(path, "?", 2)[0])
	}
	var w struct {
		MediaContainer json.RawMessage
	}
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return fmt.Errorf("plex: %w", err)
	}
	return json.Unmarshal(w.MediaContainer, out)
}

type metadata struct {
	RatingKey            string `json:"ratingKey"`
	GrandparentRatingKey string `json:"grandparentRatingKey"`
	HistoryKey           string `json:"historyKey"`
	Type                 string `json:"type"`
	Title                string `json:"title"`
	GrandparentTitle     string `json:"grandparentTitle"`
	ParentIndex          int    `json:"parentIndex"`
	Index                int    `json:"index"`
	Year                 int    `json:"year"`
	OriginallyAvailable  string `json:"originallyAvailableAt"`
	ViewedAt             int64  `json:"viewedAt"`
	Duration             int64  `json:"duration"`
	ViewOffset           int64  `json:"viewOffset"`
	GUID                 string `json:"guid"`
	Guid                 []struct {
		ID string `json:"id"`
	} `json:"Guid"`
	User struct {
		ID    any    `json:"id"`
		Title string `json:"title"`
	} `json:"User"`
	Player struct {
		Title   string `json:"title"`
		Product string `json:"product"`
		State   string `json:"state"`
	} `json:"Player"`
	Media []media `json:"Media"`
}

type media struct {
	Selected   bool   `json:"selected"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	VideoCodec string `json:"videoCodec"`
	Part       []struct {
		Stream []struct {
			StreamType   int    `json:"streamType"`
			DisplayTitle string `json:"displayTitle"`
			ColorTrc     string `json:"colorTrc"`
			DOVIPresent  bool   `json:"DOVIPresent"`
		} `json:"Stream"`
	} `json:"Part"`
}

var reLegacyGUID = regexp.MustCompile(`com\.plexapp\.agents\.(imdb|themoviedb|thetvdb)://([^?/]+)`)

// ids reads external IDs from the new agents' Guid list, or the legacy guid.
func (m *metadata) ids() (tmdb, tvdb int, imdb string) {
	for _, g := range m.Guid {
		switch {
		case strings.HasPrefix(g.ID, "imdb://"):
			imdb = strings.TrimPrefix(g.ID, "imdb://")
		case strings.HasPrefix(g.ID, "tmdb://"):
			tmdb, _ = strconv.Atoi(strings.TrimPrefix(g.ID, "tmdb://"))
		case strings.HasPrefix(g.ID, "tvdb://"):
			tvdb, _ = strconv.Atoi(strings.TrimPrefix(g.ID, "tvdb://"))
		}
	}
	if l := reLegacyGUID.FindStringSubmatch(m.GUID); l != nil {
		switch l[1] {
		case "imdb":
			if imdb == "" && strings.HasPrefix(l[2], "tt") {
				imdb = l[2]
			}
		case "themoviedb":
			if tmdb == 0 {
				tmdb, _ = strconv.Atoi(l[2])
			}
		case "thetvdb":
			if tvdb == 0 {
				tvdb, _ = strconv.Atoi(l[2])
			}
		}
	}
	return
}

// item fetches one library item with its GUIDs; nil if it is gone.
func (c *Client) item(ctx context.Context, key string, cache map[string]*metadata) *metadata {
	if key == "" {
		return nil
	}
	if m, ok := cache[key]; ok {
		return m
	}
	var mc struct{ Metadata []metadata }
	var m *metadata
	if err := c.get(ctx, "/library/metadata/"+url.PathEscape(key)+"?includeGuids=1", &mc); err == nil && len(mc.Metadata) > 0 {
		m = &mc.Metadata[0]
	}
	cache[key] = m
	return m
}

// identity builds what an entry was, preferring the library's metadata
// (with IDs) over what the history or session lists.
func (c *Client) identity(ctx context.Context, e *metadata, cache map[string]*metadata) model.Identity {
	md := c.item(ctx, e.RatingKey, cache)
	if e.Type == "episode" {
		id := model.Identity{Kind: model.KindEpisode, Title: e.GrandparentTitle, Season: e.ParentIndex,
			Episode: e.Index, EpisodeTitle: e.Title}
		showKey := e.GrandparentRatingKey
		if md != nil {
			id.Season, id.Episode, id.EpisodeTitle = md.ParentIndex, md.Index, md.Title
			if showKey == "" {
				showKey = md.GrandparentRatingKey
			}
		}
		if show := c.item(ctx, showKey, cache); show != nil {
			id.Title, id.Year = show.Title, show.Year
			id.TMDB, id.TVDB, id.IMDb = show.ids()
		}
		return id
	}
	id := model.Identity{Kind: model.KindMovie, Title: e.Title, Year: e.Year}
	if id.Year == 0 && len(e.OriginallyAvailable) >= 4 {
		id.Year, _ = strconv.Atoi(e.OriginallyAvailable[:4])
	}
	if md != nil {
		id.Title = md.Title
		if md.Year != 0 {
			id.Year = md.Year
		}
		id.TMDB, _, id.IMDb = md.ids() // a movie's TVDB id is not used
	}
	return id
}

// Plays implements source.Importer: this account's viewings that Plex
// recorded after since, oldest first. From Plex's database when it is
// readable (deleted items keep their IDs), else from the HTTP history.
func (c *Client) Plays(ctx context.Context, since time.Time) ([]source.Play, error) {
	if plays, err := c.playsFromDB(ctx, since); err == nil {
		return plays, nil
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
		return nil, err
	}
	return c.playsFromHTTP(ctx, since)
}

func (c *Client) playsFromHTTP(ctx context.Context, since time.Time) ([]source.Play, error) {
	cache := map[string]*metadata{}
	var out []source.Play
	for start := 0; ; start += 100 {
		var mc struct{ Metadata []metadata }
		q := fmt.Sprintf("/status/sessions/history/all?sort=viewedAt:desc&accountID=%d&viewedAt%%3E=%d"+
			"&X-Plex-Container-Start=%d&X-Plex-Container-Size=100", c.AccountID, since.Unix(), start)
		if err := c.get(ctx, q, &mc); err != nil {
			return nil, err
		}
		for i := range mc.Metadata {
			e := &mc.Metadata[i]
			if e.Type != "movie" && e.Type != "episode" {
				continue // music, photos
			}
			key := e.HistoryKey
			if key == "" {
				key = fmt.Sprintf("plex:%s:%d", e.RatingKey, e.ViewedAt)
			}
			viewed := time.Unix(e.ViewedAt, 0)
			dur := time.Duration(e.Duration) * time.Millisecond
			if md := c.item(ctx, e.RatingKey, cache); md != nil && md.Duration > 0 {
				dur = time.Duration(md.Duration) * time.Millisecond
			}
			out = append(out, source.Play{SourceKey: key, DeviceID: c.DeviceID, StartedAt: viewed.Add(-dur),
				EndedAt: viewed, Identity: c.identity(ctx, e, cache)})
		}
		if len(mc.Metadata) < 100 {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // oldest first
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Now is one thing playing on Plex right now.
type Now struct {
	Identity model.Identity
	Device   string
	State    model.PlayState
	Offset   time.Duration
	Duration time.Duration
	Video    *model.Video // format of the file Plex serves
}

var codecNames = map[string]string{"hevc": "HEVC", "h264": "H.264", "av1": "AV1", "vp9": "VP9", "mpeg2video": "MPEG-2", "vc1": "VC-1"}

// NowPlaying lists what this account plays on any device, playing first.
func (c *Client) NowPlaying(ctx context.Context) ([]Now, error) {
	var mc struct{ Metadata []metadata }
	if err := c.get(ctx, "/status/sessions", &mc); err != nil {
		return nil, err
	}
	cache := map[string]*metadata{}
	var out []Now
	for i := range mc.Metadata {
		m := &mc.Metadata[i]
		if m.Type != "movie" && m.Type != "episode" {
			continue
		}
		if fmt.Sprint(m.User.ID) != strconv.Itoa(c.AccountID) && (c.User == "" || m.User.Title != c.User) {
			continue // other accounts on the server
		}
		n := Now{Identity: c.identity(ctx, m, cache), Device: m.Player.Title,
			Offset: time.Duration(m.ViewOffset) * time.Millisecond, Duration: time.Duration(m.Duration) * time.Millisecond,
			State: model.StatePlaying, Video: videoOf(m.Media)}
		if n.Device == "" {
			n.Device = m.Player.Product
		}
		if m.Player.State == "paused" {
			n.State = model.StatePaused
		}
		out = append(out, n)
	}
	for i := 1; i < len(out); i++ { // playing before paused, stable
		for j := i; j > 0 && out[j].State == model.StatePlaying && out[j-1].State != model.StatePlaying; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func videoOf(ms []media) *model.Video {
	if len(ms) == 0 {
		return nil
	}
	m := ms[0]
	for _, x := range ms {
		if x.Selected {
			m = x
		}
	}
	v := &model.Video{Width: m.Width, Height: m.Height, Codec: codecNames[m.VideoCodec]}
	if v.Codec == "" {
		v.Codec = strings.ToUpper(m.VideoCodec)
	}
	if len(m.Part) > 0 {
		for _, s := range m.Part[0].Stream {
			if s.StreamType != 1 {
				continue
			}
			// Plex writes Dolby Vision as "DoVi" ("4K DoVi/HDR10"); the
			// base layer's HDR10 is not what plays on a DV screen.
			if s.DOVIPresent || strings.Contains(s.DisplayTitle, "DoVi") {
				v.HDR = "Dolby Vision"
				break
			}
			for _, t := range []string{"Dolby Vision", "HDR10+", "HDR10", "HLG"} {
				if strings.Contains(s.DisplayTitle, t) {
					v.HDR = t
					break
				}
			}
			if v.HDR == "" {
				switch {
				case s.ColorTrc == "smpte2084":
					v.HDR = "HDR10"
				case s.ColorTrc == "arib-std-b67":
					v.HDR = "HLG"
				}
			}
			break
		}
	}
	if v.HDR == "" && v.Width > 0 {
		v.HDR = "SDR"
	}
	return v
}
