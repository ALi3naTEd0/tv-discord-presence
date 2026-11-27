package api

import (
	"encoding/json"
	"errors"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// readJSON decodes a write's body. Writes must be application/json: a page
// on another site cannot send that to this server without a CORS preflight,
// which it never gets, so a local Couchlog is safe from cross-site posts.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" {
		http.Error(w, "send application/json", http.StatusUnsupportedMediaType)
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, msg string) { http.Error(w, msg, http.StatusBadRequest) }

func (a *API) needIdent(w http.ResponseWriter) bool {
	if a.Ident == nil {
		http.Error(w, "identification needs a TMDB key", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// Result is a title in the picker.
type Result struct {
	Kind     string `json:"kind"` // movie | tv
	TMDB     int    `json:"tmdb"`
	Title    string `json:"title"`
	Original string `json:"original_title,omitempty"`
	Year     int    `json:"year,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Overview string `json:"overview,omitempty"`
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	if !a.needIdent(w) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		badRequest(w, "q: a title, an IMDb id or a TMDB link")
		return
	}
	ts, err := a.Ident.Search(r.Context(), q)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := []Result{}
	for _, t := range ts {
		if t.Kind != identify.Movie && t.Kind != identify.Show {
			continue
		}
		out = append(out, Result{Kind: string(t.Kind), TMDB: t.TMDB, Title: t.Title, Original: t.OriginalTitle,
			Year: t.Year, Poster: t.Poster, Overview: t.Overview})
	}
	writeJSON(w, out)
}

// Show is a series for the episode picker.
type Show struct {
	TMDB    int      `json:"tmdb"`
	Title   string   `json:"title"`
	Year    int      `json:"year,omitempty"`
	Poster  string   `json:"poster,omitempty"`
	Seasons []Season `json:"seasons"`
}

// Season is one season and how many episodes it has.
type Season struct {
	Number   int `json:"number"`
	Episodes int `json:"episodes"`
}

// Episode is one episode in a season.
type Episode struct {
	Number  int    `json:"number"`
	Name    string `json:"name,omitempty"`
	Runtime int    `json:"runtime_min,omitempty"`
	AirDate string `json:"air_date,omitempty"`
	Watched bool   `json:"watched,omitempty"` // in your history already
}

func pathInt(r *http.Request, name string) (int, bool) {
	n, err := strconv.Atoi(r.PathValue(name))
	return n, err == nil && n >= 0
}

func (a *API) show(w http.ResponseWriter, r *http.Request) {
	if !a.needIdent(w) {
		return
	}
	id, ok := pathInt(r, "tmdb")
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, err := a.Ident.Catalog.Get(r.Context(), identify.Show, id)
	if errors.Is(err, identify.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	out := Show{TMDB: t.TMDB, Title: t.Title, Year: t.Year, Poster: t.Poster, Seasons: []Season{}}
	for _, s := range t.Seasons {
		out.Seasons = append(out.Seasons, Season{Number: s.Number, Episodes: s.Episodes})
	}
	writeJSON(w, out)
}

func (a *API) season(w http.ResponseWriter, r *http.Request) {
	if !a.needIdent(w) {
		return
	}
	id, ok1 := pathInt(r, "tmdb")
	n, ok2 := pathInt(r, "n")
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return
	}
	eps, err := a.Ident.Catalog.Season(r.Context(), id, n)
	if err != nil && !errors.Is(err, identify.ErrNotFound) {
		a.fail(w, err)
		return
	}
	seen, err := a.Store.WatchedEpisodes(r.Context(), store.LocalUserID, id)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := []Episode{}
	for _, e := range eps {
		out = append(out, Episode{Number: e.Number, Name: e.Name, Runtime: e.Runtime, AirDate: e.AirDate,
			Watched: seen[[2]int{n, e.Number}]})
	}
	writeJSON(w, out)
}

// IdentifyRequest says what a session was.
type IdentifyRequest struct {
	Kind     string `json:"kind"` // movie | tv
	TMDB     int    `json:"tmdb"`
	Season   int    `json:"season,omitempty"`
	Episodes []int  `json:"episodes,omitempty"` // one, or several watched in one go
	// Remember: next time this app publishes the same title, it is this
	// movie or show (the episode still comes from the device or the picker).
	Remember bool `json:"remember,omitempty"`
}

func (a *API) identifySession(w http.ResponseWriter, r *http.Request) {
	if !a.needIdent(w) {
		return
	}
	sess, ok := a.sessionFor(w, r)
	if !ok {
		return
	}
	var req IdentifyRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.TMDB <= 0 || (req.Kind != "movie" && req.Kind != "tv") {
		badRequest(w, "kind (movie|tv) and tmdb are needed")
		return
	}
	ctx := r.Context()
	var items []model.Item
	if req.Kind == "movie" {
		it, err := a.Ident.Item(ctx, identify.Movie, req.TMDB, 0, 0)
		if err != nil {
			a.identifyError(w, err)
			return
		}
		items = append(items, it)
	} else {
		if len(req.Episodes) == 0 || len(req.Episodes) > 30 || req.Season < 0 {
			badRequest(w, "a series needs a season and its episodes")
			return
		}
		for _, e := range req.Episodes {
			it, err := a.Ident.Item(ctx, identify.Show, req.TMDB, req.Season, e)
			if err != nil {
				a.identifyError(w, err)
				return
			}
			items = append(items, it)
		}
	}
	if err := a.Store.Identify(ctx, sess.ID, items, a.Sinks, a.now()); err != nil {
		a.fail(w, err)
		return
	}
	if req.Remember {
		match := sess.Hint.Title
		if req.Kind == "tv" && sess.Hint.Subtitle != "" {
			match = sess.Hint.Subtitle // HBO / Android TV: the episode name, then the show
		}
		if strings.TrimSpace(match) != "" {
			al := model.Alias{UserID: store.LocalUserID, AppID: sess.App.ID, MatchTitle: match, Identity: items[0].Identity}
			if _, err := a.Store.SaveAlias(ctx, al, a.now()); err != nil {
				a.fail(w, err)
				return
			}
		}
	}
	a.writeOne(w, r, sess.ID)
}

func (a *API) identifyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, identify.ErrNotFound):
		http.Error(w, "TMDB does not have that title", http.StatusNotFound)
	case errors.Is(err, identify.ErrNeedsEpisode):
		badRequest(w, err.Error())
	default:
		a.fail(w, err)
	}
}

func (a *API) hideSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.sessionFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Hidden bool `json:"hidden"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	h := model.Shown
	if req.Hidden {
		h = model.Ignored
	}
	if err := a.Store.SetHidden(r.Context(), sess.ID, h); err != nil {
		a.fail(w, err)
		return
	}
	a.writeOne(w, r, sess.ID)
}

// sessionFor loads the {id} session of the local user, or answers 404.
func (a *API) sessionFor(w http.ResponseWriter, r *http.Request) (*model.Session, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	sess, err := a.Store.Session(r.Context(), id)
	if errors.Is(err, store.ErrNoSession) || (err == nil && sess.UserID != store.LocalUserID) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		a.fail(w, err)
		return nil, false
	}
	return sess, true
}

func (a *API) writeOne(w http.ResponseWriter, r *http.Request, id int64) {
	sess, err := a.Store.Session(r.Context(), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	devs, err := a.Store.Devices(r.Context(), store.LocalUserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.view(r.Context(), sess, devs)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, v)
}

// SubscriptionRequest saves what a service costs; price 0 forgets it.
type SubscriptionRequest struct {
	Price    float64 `json:"price"`
	Currency string  `json:"currency"`
	Period   string  `json:"period"` // month | year
}

func (a *API) putService(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req SubscriptionRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Price < 0 || req.Price > 1e7 || math.IsNaN(req.Price) {
		badRequest(w, "price")
		return
	}
	var sub *store.Subscription
	if req.Price > 0 {
		if req.Period == "" {
			req.Period = "month"
		}
		sub = &store.Subscription{PriceCents: int64(math.Round(req.Price * 100)), Currency: strings.ToUpper(req.Currency), Period: req.Period}
	}
	if err := a.Store.SetSubscription(r.Context(), store.LocalUserID, key, sub, a.now()); err != nil {
		badRequest(w, err.Error())
		return
	}
	a.services(w, r)
}

// settings the dashboard may change
var settingKeys = map[string]bool{"currency": true}

func (a *API) putSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req struct {
		Value string `json:"value"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.HasPrefix(key, "profile:") && len(key) > len("profile:") && len(key) <= 90 {
		// your profile on a service ("profile:hbomax" = "Gordo"); "" forgets it
		v := strings.TrimSpace(req.Value)
		if len([]rune(v)) > 60 {
			badRequest(w, "profile names are up to 60 characters")
			return
		}
		if err := a.Store.SetSetting(r.Context(), store.LocalUserID, key, v); err != nil {
			a.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !settingKeys[key] || len(req.Value) > 200 {
		badRequest(w, "unknown setting")
		return
	}
	if key == "currency" && !(len(req.Value) == 3 && strings.ToUpper(req.Value) == req.Value) {
		badRequest(w, "currency: an ISO code like MXN")
		return
	}
	if err := a.Store.SetSetting(r.Context(), store.LocalUserID, key, req.Value); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
