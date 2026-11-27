package netflix

import (
	"context"
	"fmt"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Apps are the Netflix apps on the devices Couchlog listens to.
var Apps = []string{"com.netflix.ninja", "netflix"}

// Window is how far a TV session's start may be from when Netflix says the
// title started (the TV notices playback a little later, or the title was
// opened from its page a little earlier).
const Window = 5 * time.Minute

// Pair is a session and the activity entry it was.
type Pair struct {
	Session *model.Session
	Viewed  Viewed
}

// Match pairs unidentified Netflix sessions with activity entries: each
// entry goes to the session that started closest to it, within Window.
// Netflix keeps only the latest viewing of each title, so an earlier
// session of the same title finds no entry and stays for the picker.
func Match(sessions []*model.Session, viewed []Viewed) []Pair {
	used := map[int64]bool{}
	var out []Pair
	for _, v := range viewed {
		var best *model.Session
		bestGap := Window + 1
		for _, s := range sessions {
			if used[s.ID] {
				continue
			}
			gap := s.StartedAt.Sub(v.Started())
			if gap < 0 {
				gap = -gap
			}
			if gap <= Window && gap < bestGap {
				best, bestGap = s, gap
			}
		}
		if best != nil {
			used[best.ID] = true
			out = append(out, Pair{Session: best, Viewed: v})
		}
	}
	return out
}

// Resolve turns an activity entry into an item: Netflix's metadata gives
// the English title, the year and the real season and episode numbers, and
// TMDB the IDs (only an exact, unique match). nil when unsure.
func Resolve(ctx context.Context, c *Client, id *identify.Identifier, v Viewed) (*model.Item, error) {
	if v.Series != 0 {
		md, err := c.Metadata(ctx, v.Series)
		if err != nil {
			return nil, err
		}
		if md.Type != "show" || md.HiddenEpisodeNumbers {
			return nil, nil // collections number episodes differently from TMDB
		}
		name := md.Title
		if name == "" {
			name = v.SeriesTitle
		}
		show, err := id.Unique(ctx, name, identify.Show, md.Year)
		if err != nil || show == nil {
			return nil, err
		}
		for _, s := range md.Seasons {
			for _, e := range s.Episodes {
				if e.ID == v.MovieID {
					it, err := id.Item(ctx, identify.Show, show.TMDB, s.Seq, e.Seq)
					if err != nil {
						return nil, err
					}
					return &it, nil
				}
			}
		}
		return nil, fmt.Errorf("netflix: episode %d not in %q", v.MovieID, name)
	}
	md, err := c.Metadata(ctx, v.MovieID)
	if err != nil {
		return nil, err
	}
	name := md.Title
	if name == "" {
		name = v.Title
	}
	mv, err := id.Unique(ctx, name, identify.Movie, md.Year)
	if err != nil || mv == nil {
		return nil, err
	}
	it, err := id.Item(ctx, identify.Movie, mv.TMDB, 0, 0)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// ContinueAfter is how old a session with no entry of its own must be to be
// taken as the newest entry's title. Netflix lists a new title within a few
// minutes (about 4 seen), so by then a new one would have its own entry.
const ContinueAfter = 6 * time.Minute

// Continuations pairs sessions Match left alone with the newest activity
// entry when that entry is from before them (within 6 h) and they are
// ContinueAfter old: Netflix keeps one entry per title, so picking a title up
// again or restarting it adds no new entry, while a new title would have
// its own by then.
func Continuations(sessions []*model.Session, viewed []Viewed, matched []Pair, now time.Time) []Pair {
	if len(viewed) == 0 {
		return nil
	}
	newest := viewed[0]
	for _, v := range viewed {
		if v.DateMS > newest.DateMS {
			newest = v
		}
	}
	used := map[int64]bool{}
	for _, p := range matched {
		used[p.Session.ID] = true
	}
	var out []Pair
	for _, s := range sessions {
		since := s.StartedAt.Sub(newest.Started())
		if used[s.ID] || since <= Window || since > 6*time.Hour || now.Sub(s.StartedAt) < ContinueAfter {
			continue
		}
		out = append(out, Pair{Session: s, Viewed: newest})
	}
	return out
}
