package identify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// IMDb fills in posters TMDB does not have yet: brand-new films and series
// (TMDB adds the poster days later) and titles TMDB does not list at all.
// It uses IMDb's public suggestion endpoint and only ever gives a poster,
// never an ID: an ID guessed by title could send the wrong thing to Trakt.
type IMDb struct {
	BaseURL string // "" = https://v3.sg.media-imdb.com
	HTTP    *http.Client

	mu    sync.Mutex
	cache map[string]imdbCached
}

type imdbCached struct {
	results []imdbSuggestion
	at      time.Time
}

type imdbSuggestion struct {
	ID    string `json:"id"`
	Title string `json:"l"`
	Year  int    `json:"y"`
	Kind  string `json:"qid"` // movie, tvSeries, tvMiniSeries, short, ...
	Image struct {
		URL string `json:"imageUrl"`
	} `json:"i"`
}

var imdbKinds = map[string]Kind{
	"movie": Movie, "short": Movie, "tvMovie": Movie, "video": Movie, "tvSpecial": Movie, "tvShort": Movie,
	"tvSeries": Show, "tvMiniSeries": Show,
}

var reIMDbSize = regexp.MustCompile(`\._V1_.*\.jpg$`)

// posterSize asks IMDb's image server for a 300 px wide poster.
func posterSize(u string) string {
	if strings.Contains(u, "._V1_") {
		return reIMDbSize.ReplaceAllString(u, "._V1_QL75_UX300_.jpg")
	}
	return u
}

func (m *IMDb) suggest(ctx context.Context, q string) ([]imdbSuggestion, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	m.mu.Lock()
	if c, ok := m.cache[q]; ok && time.Since(c.at) < 6*time.Hour {
		m.mu.Unlock()
		return c.results, nil
	}
	m.mu.Unlock()
	base := m.BaseURL
	if base == "" {
		base = "https://v3.sg.media-imdb.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/suggestion/x/"+url.PathEscape(q)+".json", nil)
	if err != nil {
		return nil, err
	}
	hc := m.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r struct {
		D []imdbSuggestion `json:"d"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	if m.cache == nil {
		m.cache = map[string]imdbCached{}
	}
	m.cache[q] = imdbCached{results: r.D, at: time.Now()}
	m.mu.Unlock()
	return r.D, nil
}

// Poster finds a poster on IMDb: by IMDb id when there is one, otherwise
// only a single exact match by title, year (when known) and kind. "" when
// there is none.
func (m *IMDb) Poster(ctx context.Context, kind model.ItemKind, title string, year int, imdbID string) string {
	q := imdbID
	if q == "" {
		q = title
	}
	if q == "" {
		return ""
	}
	res, err := m.suggest(ctx, q)
	if err != nil {
		return ""
	}
	want := Movie
	if kind == model.KindEpisode {
		want = Show
	}
	var hits []string
	for _, r := range res {
		if r.Image.URL == "" {
			continue
		}
		if imdbID != "" {
			if r.ID == imdbID {
				return posterSize(r.Image.URL)
			}
			continue
		}
		if imdbKinds[r.Kind] == want && store.NormalizeTitle(r.Title) == store.NormalizeTitle(title) &&
			(year == 0 || r.Year == year) {
			hits = append(hits, r.Image.URL)
		}
	}
	if len(hits) == 1 {
		return posterSize(hits[0])
	}
	return ""
}

// Poster returns a poster for an item that has none: TMDB again (posters
// arrive days after a listing), then IMDb. "" when neither has one.
func (id *Identifier) Poster(ctx context.Context, it model.Item) string {
	if it.TMDB != 0 && id.Catalog != nil {
		k := Movie
		if it.Kind == model.KindEpisode {
			k = Show
		}
		if t, err := id.Catalog.Get(ctx, k, it.TMDB); err == nil && t.Poster != "" {
			return t.Poster
		}
	}
	if id.Posters != nil {
		return id.Posters.Poster(ctx, it.Kind, it.Title, it.Year, it.IMDb)
	}
	return ""
}
