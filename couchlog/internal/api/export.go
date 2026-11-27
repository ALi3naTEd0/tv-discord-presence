package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// exportDays reads ?days=N (default: everything).
func exportSince(r *http.Request, now time.Time) (time.Time, bool) {
	v := r.URL.Query().Get("days")
	if v == "" || v == "all" {
		return time.Time{}, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 36500 {
		return time.Time{}, false
	}
	return now.AddDate(0, 0, -n), true
}

func csvHeaders(w http.ResponseWriter, name string) *csv.Writer {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Cache-Control", "no-store")
	return csv.NewWriter(w)
}

// letterboxd writes the movies you watched in Letterboxd's import format
// (letterboxd.com/import): one row per viewing, matched by IMDb / TMDB id,
// "Rewatch" when the same film was watched before.
func (a *API) letterboxd(w http.ResponseWriter, r *http.Request) {
	since, ok := exportSince(r, a.now())
	if !ok {
		badRequest(w, "days: a number of days, or all")
		return
	}
	ps, err := a.loadPlayed(r.Context(), since)
	if err != nil {
		a.fail(w, err)
		return
	}
	type row struct {
		it model.Item
		at time.Time
	}
	var rows []row
	for _, p := range ps {
		if p.sess.Open {
			continue
		}
		for _, it := range p.items {
			if it.Kind == model.KindMovie && finishedEnough(p.sess, it) {
				rows = append(rows, row{it, p.sess.StartedAt})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
	cw := csvHeaders(w, "couchlog-letterboxd.csv")
	cw.Write([]string{"Title", "Year", "imdbID", "tmdbID", "WatchedDate", "Rewatch"})
	seen := map[string]bool{}
	lastDay := map[string]string{}
	for _, x := range rows {
		key := x.it.IMDb
		if key == "" {
			key = strconv.Itoa(x.it.TMDB) + "|" + x.it.Title
		}
		day := x.at.Local().Format("2006-01-02")
		if lastDay[key] == day {
			continue // one film picked up over several sessions the same day is one viewing
		}
		rewatch := "false"
		if seen[key] {
			rewatch = "true"
		}
		seen[key], lastDay[key] = true, day
		year, tmdb := "", ""
		if x.it.Year > 0 {
			year = strconv.Itoa(x.it.Year)
		}
		if x.it.TMDB > 0 {
			tmdb = strconv.Itoa(x.it.TMDB)
		}
		cw.Write([]string{x.it.Title, year, x.it.IMDb, tmdb, day, rewatch})
	}
	cw.Flush()
}

// finishedEnough: a film counts for Letterboxd once you watched most of it
// (80 % of a known runtime), or when the runtime is unknown.
func finishedEnough(s *model.Session, it model.Item) bool {
	if it.RuntimeMinutes == 0 {
		return true
	}
	played := s.Played
	if s.Position != nil && *s.Position > played {
		played = *s.Position
	}
	return played >= time.Duration(float64(it.RuntimeMinutes)*0.8*float64(time.Minute))
}

// historyCSV writes every session (identified or not) with its titles,
// format and where each title stands with each destination.
func (a *API) historyCSV(w http.ResponseWriter, r *http.Request) {
	since, ok := exportSince(r, a.now())
	if !ok {
		badRequest(w, "days: a number of days, or all")
		return
	}
	list, err := a.Store.SessionsSince(r.Context(), store.LocalUserID, since)
	if err != nil {
		a.fail(w, err)
		return
	}
	devs, err := a.Store.Devices(r.Context(), store.LocalUserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	cw := csvHeaders(w, "couchlog-history.csv")
	cw.Write([]string{"started", "ended", "minutes", "device", "app", "hidden", "kind", "title", "year", "season",
		"episode", "episode_title", "imdb", "tmdb", "tvdb", "quality", "app_said", "trakt", "simkl"})
	sort.Slice(list, func(i, j int) bool { return list[i].StartedAt.Before(list[j].StartedAt) })
	for _, s := range list {
		ended := ""
		if s.EndedAt != nil {
			ended = s.EndedAt.Local().Format(time.RFC3339)
		}
		q := ""
		if qq := QualityOf(s.Video); qq != nil {
			q = qq.Label
		}
		said := s.Hint.Title
		if s.Hint.Subtitle != "" {
			said += " · " + s.Hint.Subtitle
		}
		base := []string{s.StartedAt.Local().Format(time.RFC3339), ended, strconv.Itoa(int(s.Played.Minutes())),
			devs[s.DeviceID].Name, s.App.Name, string(s.Hidden)}
		items, err := a.Store.Items(r.Context(), s.ID)
		if err != nil {
			a.fail(w, err)
			return
		}
		if len(items) == 0 {
			cw.Write(append(base, "", "", "", "", "", "", "", "", "", q, said, "", ""))
			continue
		}
		for _, it := range items {
			dels, _ := a.Store.Deliveries(r.Context(), it.ID)
			st := map[string]string{}
			for _, d := range dels {
				st[d.Sink] = string(d.Status)
			}
			num := func(n int) string {
				if n == 0 {
					return ""
				}
				return strconv.Itoa(n)
			}
			cw.Write(append(base, string(it.Kind), it.Title, num(it.Year), num(it.Season), num(it.Episode), it.EpisodeTitle,
				it.IMDb, num(it.TMDB), num(it.TVDB), q, said, st["trakt"], st["simkl"]))
		}
	}
	cw.Flush()
}
