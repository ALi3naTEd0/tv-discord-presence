package identify

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// History is what Auto needs to know about what you watched before.
// *store.Store is one.
type History interface {
	// LastEpisode is the furthest episode of a show (by TMDB id) you watched.
	LastEpisode(ctx context.Context, userID int64, tmdb int) (season, episode int, ok bool, err error)
	// RecentShow is the show of the latest identified episode in an app since a time.
	RecentShow(ctx context.Context, userID int64, appID string, since time.Time) (*model.Identity, error)
}

// YouTubeMovieChannels are the names YouTube gives its own film and series
// store: a video there is a real film, so it is identified like any app.
var YouTubeMovieChannels = map[string]bool{
	"películas y series de youtube": true,
	"youtube movies":                true,
	"youtube movies & tv":           true,
	"youtube movies and tv":         true,
	"filmes e séries do youtube":    true,
}

var (
	// "S02:E21 - Lauren", "S1 E2", "S01E02", "T1:E2", "Season 1: Episode 2",
	// "Temporada 1 Episodio 2", "1x02"
	reEpisodeRef = regexp.MustCompile(`(?i)(?:\b[st](\d{1,2})\s*[:.\-]?\s*e(?:p|pisode|pisodio)?\.?\s*(\d{1,3})\b|` +
		`\b(?:season|temporada)\s*(\d{1,2})\s*[:,.\-]?\s*(?:episode|episodio|capítulo|cap\.?|ep\.?)\s*(\d{1,3})\b|\b(\d{1,2})x(\d{2,3})\b)`)
	rePlaceholder = regexp.MustCompile(`(?i)^\s*(?:episode|episodio|capítulo|chapter|ep\.?)\s*\d+\s*$|^\s*(?:tba|tbd)?\s*$`)
)

// parseEpisodeRef finds a season/episode reference and returns the text
// around it (the episode's name, or the show's) with separators trimmed.
func parseEpisodeRef(s string) (season, episode int, rest string, ok bool) {
	m := reEpisodeRef.FindStringSubmatchIndex(s)
	if m == nil {
		return 0, 0, s, false
	}
	g := func(i int) int {
		if m[2*i] < 0 {
			return -1
		}
		n, _ := strconv.Atoi(s[m[2*i]:m[2*i+1]])
		return n
	}
	for _, p := range [][2]int{{1, 2}, {3, 4}, {5, 6}} {
		if g(p[0]) >= 0 {
			season, episode = g(p[0]), g(p[1])
		}
	}
	rest = strings.Trim(s[:m[0]]+" "+s[m[1]:], " -–—:|·,.")
	return season, episode, strings.Join(strings.Fields(rest), " "), true
}

// Auto identifies a session from what its device published, when that is
// certain enough to send without asking. In order:
//
//  1. an alias you made for that name;
//  2. a season/episode in the text ("S02:E21 - Lauren", "Season 1: Episode
//     2"): the show is the other text, or, with no show name (Tubi), the show
//     you were watching in that app if that episode's name matches;
//  3. no episode: a film whose name is the title, the only one with it;
//  4. or a show whose name is the subtitle and whose episode has the title
//     as its name (HBO publishes "episode name / show"). When the show's
//     episodes have no names yet (new series: "Episode 1"), the episode
//     after your last one is a guess, ByProgress, not to be sent unconfirmed.
//
// Anything less certain is left for the user: a wrong guess reaches Trakt.
// YouTube titles are video names, so there only aliases and YouTube's own
// film store count.
func (id *Identifier) Auto(ctx context.Context, userID int64, s *model.Session) (*model.Item, Method, error) {
	h := s.Hint
	title, sub := strings.TrimSpace(h.Title), strings.TrimSpace(h.Subtitle)
	if title == "" {
		return nil, "", nil
	}
	season, episode := h.Season, h.Episode
	hasEp := season > 0 || episode > 0
	name, epName := title, ""
	if !hasEp {
		if se, ep, rest, ok := parseEpisodeRef(title); ok {
			season, episode, hasEp, epName, name = se, ep, true, rest, ""
		} else if se, ep, rest, ok := parseEpisodeRef(sub); ok {
			season, episode, hasEp, epName, sub = se, ep, true, title, rest
			name = ""
		}
	}

	// 1. Aliases, for every name the device gave.
	if id.Aliases != nil {
		for _, n := range []string{title, sub, name} {
			if n == "" {
				continue
			}
			a, err := id.Aliases.FindAlias(ctx, userID, s.App.ID, n, 0)
			if err != nil {
				return nil, "", err
			}
			if a == nil || a.TMDB == 0 {
				continue
			}
			if a.Kind == model.KindMovie {
				it, err := id.Item(ctx, Movie, a.TMDB, 0, 0)
				if err != nil {
					return nil, "", err
				}
				return &it, ByAlias, nil
			}
			return id.episodeOf(ctx, userID, a.TMDB, season, episode, epName, title, ByAlias)
		}
	}

	noGuess := id.NoGuessApps
	if noGuess == nil {
		noGuess = DefaultNoGuessApps
	}
	if noGuess[s.App.ID] {
		if !YouTubeMovieChannels[strings.ToLower(sub)] {
			return nil, "", nil
		}
		hasEp, sub = false, "" // YouTube's film store: the title is a film
	}

	// 2. Season and episode known.
	if hasEp {
		for _, n := range []string{name, sub} {
			if n == "" {
				continue
			}
			show, err := id.unique(ctx, n, Show)
			if err != nil || show == nil {
				if err != nil {
					return nil, "", err
				}
				continue
			}
			it, err := id.Item(ctx, Show, show.TMDB, season, episode)
			if err != nil {
				return nil, "", err
			}
			return &it, ByMatch, nil
		}
		// No show name (Tubi): the show you were watching in this app.
		if id.History != nil && epName != "" {
			prev, err := id.History.RecentShow(ctx, userID, s.App.ID, time.Now().AddDate(0, 0, -30))
			if err != nil {
				return nil, "", err
			}
			if prev != nil && prev.TMDB != 0 {
				it, err := id.Item(ctx, Show, prev.TMDB, season, episode)
				if err == nil && it.EpisodeTitle != "" && store.NormalizeTitle(it.EpisodeTitle) == store.NormalizeTitle(epName) {
					return &it, ByContinuation, nil
				}
			}
		}
		return nil, "", nil
	}

	// 3. A film by its exact name.
	// A subtitle that names a show means the title is an episode's name.
	mv, err := id.unique(ctx, title, Movie)
	if err != nil {
		return nil, "", err
	}
	if mv != nil && (sub == "" || id.unknownShow(ctx, sub)) {
		it, err := id.Item(ctx, Movie, mv.TMDB, 0, 0)
		if err != nil {
			return nil, "", err
		}
		return &it, ByMatch, nil
	}

	// 4. "Episode name / show".
	if sub != "" {
		show, err := id.unique(ctx, sub, Show)
		if err != nil {
			return nil, "", err
		}
		if show != nil {
			return id.episodeOf(ctx, userID, show.TMDB, 0, 0, title, title, ByMatch)
		}
	}
	return nil, "", nil
}

// unknownShow reports that a subtitle does not name a show (so a title that
// is also a film's name is that film, not an episode called like it).
func (id *Identifier) unknownShow(ctx context.Context, sub string) bool {
	show, err := id.unique(ctx, sub, Show)
	return err == nil && show == nil
}

// unique is the only TMDB title of that kind whose name (or original name)
// is n; nil when there is none or more than one.
func (id *Identifier) unique(ctx context.Context, n string, k Kind) (*Title, error) {
	found, err := id.Catalog.Search(ctx, n)
	if err != nil {
		return nil, err
	}
	want := store.NormalizeTitle(n)
	var match *Title
	for i, t := range found {
		if t.Kind != k {
			continue
		}
		if store.NormalizeTitle(t.Title) != want && store.NormalizeTitle(t.OriginalTitle) != want {
			continue
		}
		if match != nil {
			return nil, nil // a remake, or two shows with that name: ask
		}
		match = &found[i]
	}
	return match, nil
}

// episodeOf builds the episode of a known show: by number when the device
// gave one, else by the episode's name, else (names not listed yet) the
// episode after your last one, as a guess.
func (id *Identifier) episodeOf(ctx context.Context, userID int64, show, season, episode int, epName, fallbackName string, how Method) (*model.Item, Method, error) {
	if episode > 0 {
		it, err := id.Item(ctx, Show, show, season, episode)
		if err != nil {
			return nil, "", err
		}
		return &it, how, nil
	}
	if epName == "" {
		epName = fallbackName
	}
	t, err := id.Catalog.Get(ctx, Show, show)
	if err != nil {
		return nil, "", err
	}
	want := store.NormalizeTitle(epName)
	type ref struct{ s, e int }
	var named []ref
	placeholders := true
	for _, se := range t.Seasons {
		if se.Episodes == 0 {
			continue
		}
		eps, err := id.Catalog.Season(ctx, show, se.Number)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, "", err
		}
		for _, e := range eps {
			if !rePlaceholder.MatchString(e.Name) {
				placeholders = false
			}
			if want != "" && store.NormalizeTitle(e.Name) == want {
				named = append(named, ref{se.Number, e.Number})
			}
		}
	}
	if len(named) == 1 {
		it, err := id.Item(ctx, Show, show, named[0].s, named[0].e)
		if err != nil {
			return nil, "", err
		}
		return &it, how, nil
	}
	if len(named) > 1 || !placeholders {
		return nil, "", nil // the name is ambiguous, or not this show's
	}
	// The show lists no episode names yet: guess the one after your last.
	next := model.Item{Identity: model.Identity{Kind: model.KindEpisode, TMDB: show, Season: 1, Episode: 0}}
	if id.History != nil {
		if s, e, ok, err := id.History.LastEpisode(ctx, userID, show); err != nil {
			return nil, "", err
		} else if ok {
			next.Season, next.Episode = s, e
		}
	}
	var it model.Item
	if next.Episode == 0 {
		it, err = id.Item(ctx, Show, show, 1, 1)
	} else {
		it, err = id.Next(ctx, next)
	}
	if err != nil {
		return nil, "", nil
	}
	return &it, ByProgress, nil
}
