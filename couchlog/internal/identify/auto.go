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
			if !hasEp && sub == "" && n == title {
				// The app published only the show's name (Disney+): which
				// episode is unknown, so the one after your last is a guess
				// you confirm.
				return id.guessNext(ctx, userID, a.TMDB)
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
			show, err := id.Unique(ctx, n, Show, 0)
			if err != nil || show == nil {
				if err != nil {
					return nil, "", err
				}
				continue
			}
			return id.episodeOf(ctx, userID, show.TMDB, season, episode, epName, "", ByMatch)
		}
		// No show name (Tubi): the show you were watching in this app.
		if id.History != nil && epName != "" {
			prev, err := id.History.RecentShow(ctx, userID, s.App.ID, time.Now().AddDate(0, 0, -30))
			if err != nil {
				return nil, "", err
			}
			if prev != nil && prev.TMDB != 0 {
				se, ep, ok, err := id.numberOrName(ctx, prev.TMDB, season, episode, epName, true)
				if err != nil {
					return nil, "", err
				}
				if ok {
					it, err := id.Item(ctx, Show, prev.TMDB, se, ep)
					if err == nil {
						return &it, ByContinuation, nil
					}
				}
			}
		}
		return nil, "", nil
	}

	// 3. A film by its exact name.
	// A subtitle that names a show means the title is an episode's name.
	mv, err := id.Unique(ctx, title, Movie, 0)
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
		show, err := id.Unique(ctx, sub, Show, 0)
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
	show, err := id.Unique(ctx, sub, Show, 0)
	return err == nil && show == nil
}

// Unique is the only TMDB title of that kind whose name (or original name)
// is n (and, when several share it, whose year is year); nil when there is
// none or still more than one.
func (id *Identifier) Unique(ctx context.Context, n string, k Kind, year int) (*Title, error) {
	found, err := id.Catalog.Search(ctx, n)
	if err != nil {
		return nil, err
	}
	want := store.NormalizeTitle(n)
	var hits []*Title
	for i, t := range found {
		if t.Kind == k && (store.NormalizeTitle(t.Title) == want || store.NormalizeTitle(t.OriginalTitle) == want) {
			hits = append(hits, &found[i])
		}
	}
	if len(hits) > 1 && year != 0 {
		var same []*Title
		for _, h := range hits {
			if h.Year == year {
				same = append(same, h)
			}
		}
		hits = same
	}
	if len(hits) != 1 {
		return nil, nil // none, or a remake / two shows with that name: ask
	}
	return hits[0], nil
}

// episodeOf builds the episode of a known show: by number when the device
// gave one (checked against the episode's name when it gave that too), else
// by the episode's name, else (names not listed yet) the episode after your
// last one, as a guess.
func (id *Identifier) episodeOf(ctx context.Context, userID int64, show, season, episode int, epName, fallbackName string, how Method) (*model.Item, Method, error) {
	if episode > 0 {
		s, e, ok, err := id.numberOrName(ctx, show, season, episode, epName, false)
		if err != nil || !ok {
			return nil, "", err
		}
		it, err := id.Item(ctx, Show, show, s, e)
		if err != nil {
			return nil, "", err
		}
		return &it, how, nil
	}
	if epName == "" {
		epName = fallbackName
	}
	s, e, found, placeholders, err := id.byName(ctx, show, epName)
	if err != nil {
		return nil, "", err
	}
	if found {
		it, err := id.Item(ctx, Show, show, s, e)
		if err != nil {
			return nil, "", err
		}
		return &it, how, nil
	}
	if !placeholders {
		return nil, "", nil // the name is ambiguous, or not this show's
	}
	// The show lists no episode names yet: guess the one after your last.
	return id.guessNext(ctx, userID, show)
}

// guessNext is the episode after your last one of show (S1E1 if none), as
// a guess (ByProgress) that waits for you to confirm it.
func (id *Identifier) guessNext(ctx context.Context, userID int64, show int) (*model.Item, Method, error) {
	var err error
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

// numberOrName resolves what an app published as "S03:E02 - Kim". Apps
// number episodes their own way (Tubi's S3E2 "Kim" is S3E3 on TMDB), so the
// name wins when it names one episode: in the app's season first (common
// names like "Ryan" repeat across seasons), then in the whole show. The
// number is trusted alone only with no name to check, or while the show
// lists no names yet. needName: the show itself is a guess (the one last
// watched in the app), so the name must match.
func (id *Identifier) numberOrName(ctx context.Context, show, season, episode int, epName string, needName bool) (int, int, bool, error) {
	want := store.NormalizeTitle(epName)
	if want == "" {
		return season, episode, !needName, nil
	}
	eps, err := id.Catalog.Season(ctx, show, season)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, 0, false, err
	}
	var hits []int
	for _, e := range eps {
		if store.NormalizeTitle(e.Name) == want {
			hits = append(hits, e.Number)
		}
	}
	if len(hits) == 1 {
		return season, hits[0], true, nil
	}
	s, e, found, placeholders, err := id.byName(ctx, show, epName)
	switch {
	case err != nil:
		return 0, 0, false, err
	case found:
		return s, e, true, nil
	case placeholders && !needName:
		return season, episode, true, nil
	}
	return 0, 0, false, nil
}

// byName finds the one episode of a show called name: exactly, or clearly
// closest when apps and TMDB word it differently ("In Your Own Time" / "In
// Your Time"). placeholders: the show lists no episode names yet.
func (id *Identifier) byName(ctx context.Context, show int, name string) (season, episode int, found, placeholders bool, err error) {
	t, err := id.Catalog.Get(ctx, Show, show)
	if err != nil {
		return 0, 0, false, false, err
	}
	want := store.NormalizeTitle(name)
	type ref struct{ s, e int }
	type cand struct {
		ref
		name string
	}
	var named []ref
	var all []cand
	placeholders = true
	for _, se := range t.Seasons {
		if se.Episodes == 0 {
			continue
		}
		eps, err := id.Catalog.Season(ctx, show, se.Number)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return 0, 0, false, false, err
		}
		for _, e := range eps {
			if !rePlaceholder.MatchString(e.Name) {
				placeholders = false
				all = append(all, cand{ref{se.Number, e.Number}, store.NormalizeTitle(e.Name)})
			}
			if want != "" && store.NormalizeTitle(e.Name) == want {
				named = append(named, ref{se.Number, e.Number})
			}
		}
	}
	if len(named) == 0 && want != "" && !placeholders {
		best, second, at := 0.0, 0.0, ref{}
		for _, c := range all {
			r := titleSimilarity(want, c.name)
			if r > best {
				best, second, at = r, best, c.ref
			} else if r > second {
				second = r
			}
		}
		if best >= 0.8 && second < best-0.1 {
			named = append(named, at)
		}
	}
	if len(named) == 1 {
		return named[0].s, named[0].e, true, false, nil
	}
	return 0, 0, false, placeholders, nil
}

// titleSimilarity is 1 for equal normalized titles, 0.85 when one title's
// words (two or more) are all in the other, else 1 - edit distance / length.
func titleSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	wa, wb := strings.Fields(a), strings.Fields(b)
	if len(wa) >= 2 && len(wb) >= 2 && (subsetWords(wa, wb) || subsetWords(wb, wa)) {
		return 0.85
	}
	ra, rb := []rune(a), []rune(b)
	n := max(len(ra), len(rb))
	if n == 0 {
		return 0
	}
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return 1 - float64(prev[len(rb)])/float64(n)
}

func subsetWords(small, big []string) bool {
	have := map[string]bool{}
	for _, w := range big {
		have[w] = true
	}
	for _, w := range small {
		if !have[w] {
			return false
		}
	}
	return true
}
