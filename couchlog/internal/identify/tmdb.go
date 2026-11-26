package identify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultKey is Couchlog's own TMDB key, put into release builds at compile
// time from a CI secret (-ldflags "-X .../identify.DefaultKey=..."), so
// nobody installing Couchlog sees or sets one. COUCHLOG_TMDB_KEY overrides
// it (development, or your own key).
var DefaultKey string

// PosterBase is where TMDB serves posters at the size the dashboard uses.
const PosterBase = "https://image.tmdb.org/t/p/w342"

// ErrNotFound means TMDB has no such title, season or episode.
var ErrNotFound = errors.New("identify: not found on TMDB")

// ErrBadKey means TMDB rejected the key: missing, mistyped or revoked.
var ErrBadKey = errors.New("identify: TMDB rejected the API key")

// TMDB is a small client for the parts of TMDB's v3 API that Couchlog uses.
// Responses are cached for CacheTTL: titles do not change minute to minute,
// and the shared rate limit is per app, not per user.
type TMDB struct {
	// Key is either the v4 "API Read Access Token" (sent as a Bearer token)
	// or a v3 API key (sent as api_key); both work for every v3 endpoint.
	Key      string
	Language string        // e.g. "es-MX"; "" = TMDB's default (en-US)
	BaseURL  string        // "" = https://api.themoviedb.org/3; tests point it at httptest
	HTTP     *http.Client  // nil = a client with a 20 s timeout
	CacheTTL time.Duration // 0 = 6 h

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	body []byte
	at   time.Time
}

func (c *TMDB) get(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	if c.Language != "" {
		params.Set("language", c.Language)
	}
	bearer := strings.HasPrefix(c.Key, "eyJ") // a JWT: the v4 read token
	if !bearer && c.Key != "" {
		params.Set("api_key", c.Key)
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.themoviedb.org/3"
	}
	u := base + path + "?" + params.Encode()
	ttl := c.CacheTTL
	if ttl == 0 {
		ttl = 6 * time.Hour
	}

	c.mu.Lock()
	if e, ok := c.cache[u]; ok && time.Since(e.at) < ttl {
		c.mu.Unlock()
		return json.Unmarshal(e.body, out)
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("identify: TMDB %s: %w", path, err)
	}
	defer resp.Body.Close()
	var body json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil && resp.StatusCode == http.StatusOK {
		return fmt.Errorf("identify: TMDB %s: %w", path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w (%s)", ErrNotFound, path)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w (HTTP 401 on %s)", ErrBadKey, path)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("identify: TMDB %s: HTTP %d", path, resp.StatusCode)
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cached{}
	}
	c.cache[u] = cached{body: body, at: time.Now()}
	c.mu.Unlock()
	return json.Unmarshal(body, out)
}

// Kind is what a TMDB listing is: a movie or a whole show.
type Kind string

const (
	Movie Kind = "movie"
	Show  Kind = "tv"
)

// Title is one movie or show as TMDB lists it.
type Title struct {
	Kind          Kind
	TMDB          int
	Title         string
	OriginalTitle string
	Year          int
	Poster        string
	Overview      string
	IMDb          string // filled by Movie/Show, not by searches
	TVDB          int
	Runtime       int      // movie minutes, or the show's usual episode length
	Seasons       []Season // shows only
}

// Season is a show's season as the show lists it.
type Season struct {
	Number   int
	Episodes int
}

// Episode is one episode of a season.
type Episode struct {
	Number  int
	Name    string
	Runtime int // minutes, 0 if TMDB has none yet
	AirDate string
}

type listing struct {
	ID            int    `json:"id"`
	MediaType     string `json:"media_type"`
	Title         string `json:"title"`
	Name          string `json:"name"`
	OriginalTitle string `json:"original_title"`
	OriginalName  string `json:"original_name"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
	PosterPath    string `json:"poster_path"`
	Overview      string `json:"overview"`
	Runtime       int    `json:"runtime"`
	EpisodeRun    []int  `json:"episode_run_time"`
	Seasons       []struct {
		Number   int `json:"season_number"`
		Episodes int `json:"episode_count"`
	} `json:"seasons"`
	External struct {
		IMDb string `json:"imdb_id"`
		TVDB int    `json:"tvdb_id"`
	} `json:"external_ids"`
	IMDb string `json:"imdb_id"` // movies carry it at the top level too
}

func (l listing) title(k Kind) Title {
	t := Title{Kind: k, TMDB: l.ID, Title: l.Title, OriginalTitle: l.OriginalTitle,
		Year: yearOf(l.ReleaseDate), Overview: l.Overview, Runtime: l.Runtime,
		IMDb: l.External.IMDb, TVDB: l.External.TVDB}
	if k == Show {
		t.Title, t.OriginalTitle, t.Year = l.Name, l.OriginalName, yearOf(l.FirstAirDate)
		if len(l.EpisodeRun) > 0 {
			t.Runtime = l.EpisodeRun[0]
		}
	}
	if t.IMDb == "" {
		t.IMDb = l.IMDb
	}
	if l.PosterPath != "" {
		t.Poster = PosterBase + l.PosterPath
	}
	if len(t.Overview) > 240 {
		t.Overview = t.Overview[:240]
	}
	for _, s := range l.Seasons {
		t.Seasons = append(t.Seasons, Season{Number: s.Number, Episodes: s.Episodes})
	}
	sort.Slice(t.Seasons, func(i, j int) bool { return t.Seasons[i].Number < t.Seasons[j].Number })
	return t
}

func yearOf(date string) int {
	if len(date) >= 4 {
		if y, err := strconv.Atoi(date[:4]); err == nil {
			return y
		}
	}
	return 0
}

// Search finds movies and shows by title (people and the rest are left out),
// in TMDB's order, at most 10.
func (c *TMDB) Search(ctx context.Context, q string) ([]Title, error) {
	var r struct{ Results []listing }
	if err := c.get(ctx, "/search/multi", url.Values{"query": {q}, "include_adult": {"false"}}, &r); err != nil {
		return nil, err
	}
	var out []Title
	for _, l := range r.Results {
		if (l.MediaType == "movie" || l.MediaType == "tv") && len(out) < 10 {
			out = append(out, l.title(Kind(l.MediaType)))
		}
	}
	return out, nil
}

// Get returns one movie or show with its external IDs (and seasons, for a show).
func (c *TMDB) Get(ctx context.Context, k Kind, id int) (Title, error) {
	var l listing
	if err := c.get(ctx, fmt.Sprintf("/%s/%d", k, id), url.Values{"append_to_response": {"external_ids"}}, &l); err != nil {
		return Title{}, err
	}
	return l.title(k), nil
}

// Season lists one season's episodes.
func (c *TMDB) Season(ctx context.Context, show, season int) ([]Episode, error) {
	var r struct {
		Episodes []struct {
			Number  int    `json:"episode_number"`
			Name    string `json:"name"`
			Runtime int    `json:"runtime"`
			AirDate string `json:"air_date"`
		}
	}
	if err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", show, season), nil, &r); err != nil {
		return nil, err
	}
	out := make([]Episode, len(r.Episodes))
	for i, e := range r.Episodes {
		out[i] = Episode{Number: e.Number, Name: e.Name, Runtime: e.Runtime, AirDate: e.AirDate}
	}
	return out, nil
}

// Find looks a title up by an external ID: source is "imdb_id" or "tvdb_id".
func (c *TMDB) Find(ctx context.Context, id, source string) ([]Title, error) {
	var r struct {
		Movies []listing `json:"movie_results"`
		Shows  []listing `json:"tv_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(id), url.Values{"external_source": {source}}, &r); err != nil {
		return nil, err
	}
	var out []Title
	// A TVDB id is almost always a show; an IMDb id more often a movie.
	first, second := r.Movies, r.Shows
	fk, sk := Movie, Show
	if source == "tvdb_id" {
		first, second, fk, sk = r.Shows, r.Movies, Show, Movie
	}
	for _, l := range first {
		out = append(out, l.title(fk))
	}
	for _, l := range second {
		out = append(out, l.title(sk))
	}
	return out, nil
}
