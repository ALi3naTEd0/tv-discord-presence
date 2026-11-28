package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// Insights is what the period's history adds up to.
type Insights struct {
	Days       int              `json:"days"`
	From       time.Time        `json:"from"`
	FirstSeen  *time.Time       `json:"first_seen,omitempty"` // the oldest session in Couchlog
	Hours      float64          `json:"hours"`
	Sessions   int              `json:"sessions"`
	Movies     Count            `json:"movies"`
	Episodes   Count            `json:"episodes"`
	Sent       int              `json:"sent"` // items in every destination's history
	Services   []ServiceHours   `json:"services"`
	Weeks      []Week           `json:"weeks"`
	TopShows   []ShowHours      `json:"top_shows"`
	Currency   string           `json:"currency"` // the user's own, for new prices
	Subscribed map[string]Price `json:"-"`
}

// Count is a number of titles and the hours they took.
type Count struct {
	N     int     `json:"n"`
	Hours float64 `json:"hours"`
}

// ServiceHours is one service's time, and its cost when you set a price.
type ServiceHours struct {
	ServiceInfo
	Hours float64 `json:"hours"`
	Price *Price  `json:"price,omitempty"`
	// PerHour is what each hour cost in the period: the price over the
	// period (a month's price × days/30.4) divided by the hours.
	PerHour *float64 `json:"per_hour,omitempty"`
}

// Price is a saved subscription.
type Price struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Period   string  `json:"period"`
	Monthly  float64 `json:"monthly"` // a yearly price / 12
}

// Week is the hours of one calendar week (Monday to Sunday).
type Week struct {
	Start time.Time `json:"start"`
	Hours float64   `json:"hours"`
}

// ShowHours is a series and the time spent on it.
type ShowHours struct {
	TMDB     int     `json:"tmdb"`
	Title    string  `json:"title"`
	Poster   string  `json:"poster,omitempty"`
	Episodes int     `json:"episodes"`
	Hours    float64 `json:"hours"`
}

func hours(d time.Duration) float64 { return float64(int(d.Hours()*10+0.5)) / 10 }

func priceOf(s store.Subscription) *Price {
	p := &Price{Amount: float64(s.PriceCents) / 100, Currency: s.Currency, Period: s.Period}
	p.Monthly = p.Amount
	if s.Period == "year" {
		p.Monthly = p.Amount / 12
	}
	return p
}

func daysParam(r *http.Request, def int) (int, bool) {
	v := r.URL.Query().Get("days")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= 1 && n <= 3650
}

func (a *API) insights(w http.ResponseWriter, r *http.Request) {
	days, ok := daysParam(r, 30)
	if !ok {
		badRequest(w, "days must be 1-3650")
		return
	}
	in, err := a.computeInsights(r.Context(), days)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, in)
}

type played struct {
	sess  *model.Session
	items []model.Item
}

func (a *API) loadPlayed(ctx context.Context, since time.Time) ([]played, error) {
	list, err := a.Store.SessionsSince(ctx, store.LocalUserID, since)
	if err != nil {
		return nil, err
	}
	var out []played
	for _, s := range list {
		if s.Hidden != model.Shown {
			continue
		}
		items, err := a.Store.Items(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, played{s, items})
	}
	return out, nil
}

func (a *API) computeInsights(ctx context.Context, days int) (*Insights, error) {
	now := a.now()
	from := now.AddDate(0, 0, -days)
	ps, err := a.loadPlayed(ctx, from)
	if err != nil {
		return nil, err
	}
	subs, err := a.Store.Subscriptions(ctx, store.LocalUserID)
	if err != nil {
		return nil, err
	}
	cur, err := a.Store.Setting(ctx, store.LocalUserID, "currency", "MXN")
	if err != nil {
		return nil, err
	}
	in := &Insights{Days: days, From: from, Services: []ServiceHours{}, Weeks: []Week{}, TopShows: []ShowHours{}, Currency: cur}
	if t, err := a.Store.FirstSession(ctx, store.LocalUserID); err == nil && t != nil {
		in.FirstSeen = t
	}
	var total, movies, episodes time.Duration
	svc := map[string]*ServiceHours{}
	weeks := map[time.Time]time.Duration{}
	shows := map[int]*ShowHours{}
	showDur := map[int]time.Duration{}
	// a title picked up over several sessions is one title: count each once
	seenTitle := map[string]bool{}
	for _, p := range ps {
		d := p.sess.Played
		total += d
		in.Sessions++
		info := ServiceOf(p.sess.App.ID, p.sess.App.Name)
		if svc[info.Key] == nil {
			svc[info.Key] = &ServiceHours{ServiceInfo: info}
		}
		svc[info.Key].Hours += d.Hours()
		weeks[weekStart(p.sess.StartedAt)] += d
		if len(p.items) == 0 {
			continue
		}
		share := d / time.Duration(len(p.items))
		for _, it := range p.items {
			key := fmt.Sprintf("%s|%d|%s|%d|%d|%d", it.Kind, it.TMDB, it.Title, it.Year, it.Season, it.Episode)
			first := !seenTitle[key]
			seenTitle[key] = true
			if it.Kind == model.KindMovie {
				if first {
					in.Movies.N++
				}
				movies += share
			} else {
				if first {
					in.Episodes.N++
				}
				episodes += share
				if it.TMDB != 0 {
					sh := shows[it.TMDB]
					if sh == nil {
						sh = &ShowHours{TMDB: it.TMDB, Title: it.Title, Poster: it.Poster}
						shows[it.TMDB] = sh
					}
					if first {
						sh.Episodes++
					}
					showDur[it.TMDB] += share
					if sh.Poster == "" {
						sh.Poster = it.Poster
					}
				}
			}
			dels, err := a.Store.Deliveries(ctx, it.ID)
			if err != nil {
				return nil, err
			}
			if first && allSent(dels) {
				in.Sent++
			}
		}
	}
	in.Hours, in.Movies.Hours, in.Episodes.Hours = hours(total), hours(movies), hours(episodes)
	for key, s := range svc {
		s.Hours = float64(int(s.Hours*10+0.5)) / 10
		if sub, ok := subs[key]; ok {
			s.Price = priceOf(sub)
			if s.Hours > 0 {
				per := s.Price.Monthly * float64(days) / 30.4 / s.Hours
				per = float64(int(per*100+0.5)) / 100
				s.PerHour = &per
			}
		}
		in.Services = append(in.Services, *s)
	}
	// services you pay for but did not watch in the period still show: that is the point
	for key, sub := range subs {
		if svc[key] == nil {
			info, ok := knownInfo(key)
			if !ok {
				name, _ := a.Store.Setting(ctx, store.LocalUserID, "service:"+key, key)
				info = ServiceInfo{Key: key, Name: name}
			}
			in.Services = append(in.Services, ServiceHours{ServiceInfo: info, Price: priceOf(sub)})
		}
	}
	sort.Slice(in.Services, func(i, j int) bool { return in.Services[i].Hours > in.Services[j].Hours })
	for start, d := range weeks {
		in.Weeks = append(in.Weeks, Week{Start: start, Hours: hours(d)})
	}
	sort.Slice(in.Weeks, func(i, j int) bool { return in.Weeks[i].Start.Before(in.Weeks[j].Start) })
	for id, sh := range shows {
		sh.Hours = hours(showDur[id])
		in.TopShows = append(in.TopShows, *sh)
	}
	sort.Slice(in.TopShows, func(i, j int) bool { return in.TopShows[i].Hours > in.TopShows[j].Hours })
	if len(in.TopShows) > 6 {
		in.TopShows = in.TopShows[:6]
	}
	return in, nil
}

func allSent(dels []model.Delivery) bool {
	if len(dels) == 0 {
		return false
	}
	for _, d := range dels {
		if d.Status != model.Added && d.Status != model.Skipped {
			return false
		}
	}
	return true
}

// weekStart is the Monday 00:00 (local time) of t's week.
func weekStart(t time.Time) time.Time {
	t = t.Local()
	wd := (int(t.Weekday()) + 6) % 7
	y, m, d := t.Date()
	return time.Date(y, m, d-wd, 0, 0, 0, 0, time.Local)
}

// ServiceRow is one line of the Services screen.
type ServiceRow struct {
	ServiceInfo
	Hours   float64  `json:"hours"` // last 30 days
	Price   *Price   `json:"price,omitempty"`
	PerHour *float64 `json:"per_hour,omitempty"`
	Devices []string `json:"devices"` // where you watched it
	// Profile is whose profile on the service this history is ("Gordo").
	// Netflix reads it from Couchlog's Netflix connection (ProfileFrom
	// "netflix"); other services do not say, so you type it ("you").
	Profile     string `json:"profile,omitempty"`
	ProfileFrom string `json:"profile_from,omitempty"`
	Added       bool   `json:"added,omitempty"` // you added it by hand: you can remove it
}

// Services is the Services screen: every service seen plus every one with a price.
type Services struct {
	Currency string       `json:"currency"`
	Services []ServiceRow `json:"services"`
}

func (a *API) services(w http.ResponseWriter, r *http.Request) {
	in, err := a.computeInsights(r.Context(), 30)
	if err != nil {
		a.fail(w, err)
		return
	}
	devs, err := a.Store.ServiceDevices(r.Context(), store.LocalUserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	byKey := map[string][]string{}
	for appID, names := range devs {
		k := ServiceOf(appID, "").Key
		for _, n := range names {
			if !contains(byKey[k], n) {
				byKey[k] = append(byKey[k], n)
			}
		}
	}
	out := Services{Currency: in.Currency, Services: []ServiceRow{}}
	seen := map[string]bool{}
	for _, s := range in.Services {
		seen[s.Key] = true
		sort.Strings(byKey[s.Key])
		row := ServiceRow{ServiceInfo: s.ServiceInfo, Hours: s.Hours, Price: s.Price, PerHour: s.PerHour, Devices: byKey[s.Key]}
		if row.Devices == nil {
			row.Devices = []string{}
		}
		out.Services = append(out.Services, row)
	}
	// services seen before the last 30 days, with no time in them now
	for k, names := range byKey {
		if seen[k] {
			continue
		}
		info := ServiceOf(k, "")
		if info.Key != k {
			continue
		}
		out.Services = append(out.Services, ServiceRow{ServiceInfo: info, Devices: names})
	}
	// services you added yourself (one you pay for but Couchlog has not seen yet)
	added, err := a.Store.SettingsWithPrefix(r.Context(), store.LocalUserID, "service:")
	if err != nil {
		a.fail(w, err)
		return
	}
	have := map[string]int{}
	for i, x := range out.Services {
		have[x.Key] = i
	}
	for key, name := range added {
		if i, ok := have[key]; ok {
			out.Services[i].Added = true
			continue
		}
		info, ok := knownInfo(key)
		if !ok {
			info = ServiceInfo{Key: key, Name: name}
		}
		out.Services = append(out.Services, ServiceRow{ServiceInfo: info, Devices: []string{}, Added: true})
	}
	if err := a.fillProfiles(r.Context(), out.Services); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, out)
}

// fillProfiles adds each service's profile: the one you typed, else for
// Netflix the profile Couchlog's Netflix connection reads.
func (a *API) fillProfiles(ctx context.Context, rows []ServiceRow) error {
	for i := range rows {
		v, err := a.Store.Setting(ctx, store.LocalUserID, "profile:"+rows[i].Key, "")
		if err != nil {
			return err
		}
		if v != "" {
			rows[i].Profile, rows[i].ProfileFrom = v, "you"
			continue
		}
		if rows[i].Key == "netflix" && a.NetflixProfile != nil {
			pctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			name, err := a.NetflixProfile(pctx)
			cancel()
			if err == nil && name != "" {
				rows[i].Profile, rows[i].ProfileFrom = name, "netflix"
			}
		}
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// UpNext is the next episode of a show you are watching.
type UpNext struct {
	TMDB         int         `json:"tmdb"`
	Title        string      `json:"title"`
	Poster       string      `json:"poster,omitempty"`
	Season       int         `json:"season"`
	Episode      int         `json:"episode"`
	EpisodeTitle string      `json:"episode_title,omitempty"`
	Service      ServiceInfo `json:"service"` // where you watched the last one
	LastAt       time.Time   `json:"last_at"`
}

func (a *API) upNext(w http.ResponseWriter, r *http.Request) {
	out := []UpNext{}
	if a.Ident == nil {
		writeJSON(w, out)
		return
	}
	ps, err := a.loadPlayed(r.Context(), a.now().AddDate(0, 0, -30))
	if err != nil {
		a.fail(w, err)
		return
	}
	// newest first: the first episode seen per show is the latest one
	done := map[int]bool{}
	for _, p := range ps {
		for i := len(p.items) - 1; i >= 0; i-- {
			it := p.items[i]
			if it.Kind != model.KindEpisode || it.TMDB == 0 || done[it.TMDB] {
				continue
			}
			done[it.TMDB] = true
			// a session that is still open is not finished: what comes next is after it
			nx, err := a.Ident.Next(r.Context(), it)
			if err != nil || nx.Episode == 0 {
				continue
			}
			seen, err := a.Store.WatchedEpisodes(r.Context(), store.LocalUserID, it.TMDB)
			if err != nil {
				a.fail(w, err)
				return
			}
			if seen[[2]int{nx.Season, nx.Episode}] {
				continue
			}
			poster := nx.Poster
			if poster == "" {
				poster = it.Poster
			}
			out = append(out, UpNext{TMDB: it.TMDB, Title: it.Title, Poster: poster, Season: nx.Season,
				Episode: nx.Episode, EpisodeTitle: nx.EpisodeTitle, Service: ServiceOf(p.sess.App.ID, p.sess.App.Name),
				LastAt: p.sess.StartedAt})
		}
		if len(out) >= 8 {
			break
		}
	}
	writeJSON(w, out)
}
