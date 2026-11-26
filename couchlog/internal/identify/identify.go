// Package identify turns what a session was into exact IDs, using what
// already exists (TMDB for titles and IDs) instead of a catalog of its own.
//
// Three ways in:
//   - Search: the dashboard's picker, by title or by pasting an ID or link.
//   - Auto: right when a device publishes a title (Android TV): an alias the
//     user made wins; otherwise a single exact match by title. Anything less
//     certain is left for the user, because a wrong guess reaches Trakt.
//   - Next: the episode after this one, for the autoplay split.
package identify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// Catalog is where titles come from. *TMDB is the real one.
type Catalog interface {
	Search(ctx context.Context, q string) ([]Title, error)
	Get(ctx context.Context, k Kind, id int) (Title, error)
	Season(ctx context.Context, show, season int) ([]Episode, error)
	Find(ctx context.Context, id, source string) ([]Title, error)
}

// Aliases looks up the user's "this is that" corrections. *store.Store is one.
type Aliases interface {
	FindAlias(ctx context.Context, userID int64, appID, title string, year int) (*model.Alias, error)
}

// DefaultNoGuessApps publish titles that are not film or show titles
// (YouTube video names): only an alias identifies them automatically.
var DefaultNoGuessApps = map[string]bool{
	"com.google.android.youtube.tv": true,
	"youtube.leanback.v4":           true,
}

// Identifier resolves titles to items.
type Identifier struct {
	Catalog     Catalog
	Aliases     Aliases         // nil = no aliases
	NoGuessApps map[string]bool // nil = DefaultNoGuessApps
	// Posters fills posters TMDB lacks (new titles); nil = TMDB only.
	Posters *IMDb
}

// ErrNeedsEpisode means a show was picked without saying which episode.
var ErrNeedsEpisode = errors.New("identify: a show needs a season and an episode")

// Method says how Auto identified a session.
type Method string

const (
	ByAlias Method = "alias" // the user said so before
	ByMatch Method = "match" // the only exact title match on TMDB
)

// Ref is an ID or link pasted into the search box.
type Ref struct {
	Source string // "imdb", "tmdb", "tvdb"
	ID     string
	Kind   Kind // for tmdb refs only
}

var refPatterns = []struct {
	rx     *regexp.Regexp
	source string
}{
	{regexp.MustCompile(`(tt\d{6,10})`), "imdb"},
	{regexp.MustCompile(`themoviedb\.org/(movie|tv)/(\d+)`), "tmdb"},
	{regexp.MustCompile(`(?i)^tmdb[:/ ]?(movie|tv)[:/ ](\d+)$`), "tmdb"},
	{regexp.MustCompile(`(?i)(?:thetvdb\.com/\S*?[?&]id=|^tvdb[:/ ]?)(\d+)`), "tvdb"},
}

// ParseRef recognizes an IMDb id, a TMDB link or "tmdb:movie:123", and a
// TheTVDB link or "tvdb:123".
func ParseRef(q string) (Ref, bool) {
	q = strings.TrimSpace(q)
	for _, p := range refPatterns {
		m := p.rx.FindStringSubmatch(q)
		if m == nil {
			continue
		}
		if p.source == "tmdb" {
			return Ref{Source: "tmdb", Kind: Kind(strings.ToLower(m[1])), ID: m[2]}, true
		}
		return Ref{Source: p.source, ID: m[1]}, true
	}
	return Ref{}, false
}

// Search finds titles for the picker: an ID or link resolves directly,
// anything else searches by title.
func (id *Identifier) Search(ctx context.Context, q string) ([]Title, error) {
	ref, ok := ParseRef(q)
	if !ok {
		return id.Catalog.Search(ctx, q)
	}
	switch ref.Source {
	case "imdb":
		return id.Catalog.Find(ctx, ref.ID, "imdb_id")
	case "tvdb":
		return id.Catalog.Find(ctx, ref.ID, "tvdb_id")
	}
	n, _ := strconv.Atoi(ref.ID)
	t, err := id.Catalog.Get(ctx, ref.Kind, n)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []Title{t}, nil
}

// Item builds the full item for a picked title: IDs, poster, runtime and, for
// a show, the episode's title. A show needs season and episode.
func (id *Identifier) Item(ctx context.Context, k Kind, tmdbID, season, episode int) (model.Item, error) {
	if k == Show && (season < 0 || episode <= 0) {
		return model.Item{}, ErrNeedsEpisode
	}
	t, err := id.Catalog.Get(ctx, k, tmdbID)
	if err != nil {
		return model.Item{}, err
	}
	it := model.Item{
		Identity: model.Identity{Kind: model.KindMovie, Title: t.Title, Year: t.Year,
			TMDB: t.TMDB, TVDB: t.TVDB, IMDb: t.IMDb},
		Poster:         t.Poster,
		RuntimeMinutes: t.Runtime,
	}
	if it.Poster == "" && id.Posters != nil {
		kind := model.KindMovie
		if k == Show {
			kind = model.KindEpisode
		}
		it.Poster = id.Posters.Poster(ctx, kind, t.Title, t.Year, t.IMDb)
	}
	if k == Movie {
		return it, nil
	}
	it.Kind, it.Season, it.Episode = model.KindEpisode, season, episode
	// The episode's own length when TMDB has it; otherwise the show's usual
	// length, marked as estimated (auto-send does not trust an estimate).
	it.RuntimeEstimated = true
	eps, err := id.Catalog.Season(ctx, tmdbID, season)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return model.Item{}, err
	}
	for _, e := range eps {
		if e.Number == episode {
			it.EpisodeTitle = e.Name
			if e.Runtime > 0 {
				it.RuntimeMinutes, it.RuntimeEstimated = e.Runtime, false
			}
		}
	}
	if it.RuntimeMinutes == 0 {
		it.RuntimeEstimated = false
	}
	return it, nil
}

// Auto identifies a session from what its device published, when that is
// certain enough to send without asking: an alias, or exactly one TMDB title
// whose name is the published one. It returns nil when it cannot.
func (id *Identifier) Auto(ctx context.Context, userID int64, s *model.Session) (*model.Item, Method, error) {
	h := s.Hint
	if strings.TrimSpace(h.Title) == "" {
		return nil, "", nil
	}
	episode := h.Season > 0 || h.Episode > 0
	if id.Aliases != nil {
		a, err := id.Aliases.FindAlias(ctx, userID, s.App.ID, h.Title, 0)
		if err != nil {
			return nil, "", err
		}
		if a != nil && a.TMDB != 0 {
			k := Movie
			if a.Kind == model.KindEpisode {
				k = Show
			}
			it, err := id.Item(ctx, k, a.TMDB, h.Season, h.Episode)
			if errors.Is(err, ErrNeedsEpisode) {
				return nil, "", nil
			}
			if err != nil {
				return nil, "", err
			}
			return &it, ByAlias, nil
		}
	}
	noGuess := id.NoGuessApps
	if noGuess == nil {
		noGuess = DefaultNoGuessApps
	}
	if noGuess[s.App.ID] {
		return nil, "", nil
	}
	found, err := id.Catalog.Search(ctx, h.Title)
	if err != nil {
		return nil, "", err
	}
	want := store.NormalizeTitle(h.Title)
	var match *Title
	for i, t := range found {
		// Episode numbers mean a show; none means a movie (a show without
		// them cannot become an item anyway).
		if (t.Kind == Show) != episode {
			continue
		}
		if store.NormalizeTitle(t.Title) != want && store.NormalizeTitle(t.OriginalTitle) != want {
			continue
		}
		if match != nil {
			return nil, "", nil // two with that name (a remake): ask
		}
		match = &found[i]
	}
	if match == nil {
		return nil, "", nil
	}
	it, err := id.Item(ctx, match.Kind, match.TMDB, h.Season, h.Episode)
	if err != nil {
		return nil, "", err
	}
	return &it, ByMatch, nil
}

// Next returns the episode after cur: the next number in its season, or the
// first episode of the next season with episodes.
func (id *Identifier) Next(ctx context.Context, cur model.Item) (model.Item, error) {
	if cur.Kind != model.KindEpisode || cur.TMDB == 0 {
		return model.Item{}, fmt.Errorf("identify: next episode needs an identified episode")
	}
	show, err := id.Catalog.Get(ctx, Show, cur.TMDB)
	if err != nil {
		return model.Item{}, err
	}
	s, e := cur.Season, cur.Episode+1
	count := -1
	for _, x := range show.Seasons {
		if x.Number == s {
			count = x.Episodes
		}
	}
	if count >= 0 && e > count {
		s, e = -1, 1
		for _, x := range show.Seasons {
			if x.Number > cur.Season && x.Episodes > 0 {
				s = x.Number
				break
			}
		}
		if s < 0 {
			return model.Item{}, fmt.Errorf("%w: no episode after S%dE%d", ErrNotFound, cur.Season, cur.Episode)
		}
	}
	return id.Item(ctx, Show, cur.TMDB, s, e)
}
