// Package api is the dashboard's JSON API, served by the Couchlog binary.
// It turns what the store keeps into what a person reads: quality in plain
// words ("4K UHD · HEVC · Dolby Vision", no "SDR"), when what plays will end
// by the server's clock, and links to IMDb, TMDB and Trakt from the IDs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

// API serves /api/v1.
type API struct {
	Store    *store.Store
	Ident    *identify.Identifier // nil without a TMDB key: no search, identify or up next
	Version  string
	Sinks    []string // destinations set up ("trakt", "simkl")
	ReadOnly bool
	Presence string // Discord status: on | dry-run | off
	// NetflixProfile names the profile of Couchlog's Netflix connection;
	// nil when Netflix is not connected.
	NetflixProfile func(ctx context.Context) (string, error)
	Devices        DeviceHooks
	Log            *slog.Logger
	Now            func() time.Time // tests set it
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Handler returns the API's routes.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", a.status)
	mux.HandleFunc("GET /api/v1/now", a.nowPlaying)
	mux.HandleFunc("GET /api/v1/history", a.history)
	mux.HandleFunc("GET /api/v1/sessions/{id}", a.session)
	mux.HandleFunc("POST /api/v1/sessions/{id}/identify", a.identifySession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/hidden", a.hideSession)
	mux.HandleFunc("GET /api/v1/search", a.search)
	mux.HandleFunc("GET /api/v1/shows/{tmdb}", a.show)
	mux.HandleFunc("GET /api/v1/shows/{tmdb}/seasons/{n}", a.season)
	mux.HandleFunc("GET /api/v1/insights", a.insights)
	mux.HandleFunc("GET /api/v1/upnext", a.upNext)
	mux.HandleFunc("GET /api/v1/services", a.services)
	mux.HandleFunc("PUT /api/v1/services/{key}", a.putService)
	mux.HandleFunc("PUT /api/v1/settings/{key}", a.putSetting)
	mux.HandleFunc("GET /api/v1/setup/scan", a.scan)
	mux.HandleFunc("POST /api/v1/setup/devices", a.addDevice)
	mux.HandleFunc("GET /api/v1/netflix", a.netflixStatus)
	mux.HandleFunc("POST /api/v1/netflix/connect", a.connectNetflix)
	mux.HandleFunc("GET /api/v1/export/letterboxd.csv", a.letterboxd)
	mux.HandleFunc("GET /api/v1/export/history.csv", a.historyCSV)
	return mux
}

// Status is what the dashboard shows in its corner: version, clock, setup.
type Status struct {
	Version    string    `json:"version"`
	ServerTime time.Time `json:"server_time"`
	Sinks      []string  `json:"sinks"`
	ReadOnly   bool      `json:"read_only"`
	Devices    []Device  `json:"devices"`
	Identify   bool      `json:"identify"` // search and identify work (a TMDB key is set)
	// Discord is the Discord status: "on" (shown), "dry-run" (worked out,
	// not shown, while the old dashboard owns it) or "off".
	Discord string `json:"discord"`
}

// Device is a TV, box or server.
type Device struct {
	ID    int64        `json:"id"`
	Kind  string       `json:"kind"`
	Name  string       `json:"name"`
	State *DeviceState `json:"state,omitempty"` // TVs and boxes being listened to
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	devs, err := a.Store.Devices(r.Context(), store.LocalUserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	st := Status{Version: a.Version, ServerTime: a.now(), Sinks: a.Sinks, ReadOnly: a.ReadOnly, Devices: []Device{},
		Identify: a.Ident != nil, Discord: a.Presence}
	if st.Discord == "" {
		st.Discord = "off"
	}
	if st.Sinks == nil {
		st.Sinks = []string{}
	}
	var states map[int64]DeviceState
	if a.Devices.States != nil {
		states = a.Devices.States()
	}
	for _, d := range devs {
		dv := Device{ID: d.ID, Kind: d.Kind, Name: d.Name}
		if x, ok := states[d.ID]; ok {
			dv.State = &x
		}
		st.Devices = append(st.Devices, dv)
	}
	sort.Slice(st.Devices, func(i, j int) bool { return st.Devices[i].ID < st.Devices[j].ID })
	writeJSON(w, st)
}

func (a *API) nowPlaying(w http.ResponseWriter, r *http.Request) {
	sessions, err := a.Store.OpenSessions(r.Context(), store.LocalUserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeSessions(w, r.Context(), sessions, true)
}

func (a *API) history(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3650 {
			http.Error(w, "days must be 1-3650", http.StatusBadRequest)
			return
		}
		days = n
	}
	sessions, err := a.Store.SessionsSince(r.Context(), store.LocalUserID, a.now().AddDate(0, 0, -days))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeSessions(w, r.Context(), sessions, false)
}

func (a *API) session(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, err := a.Store.Session(r.Context(), id)
	if errors.Is(err, store.ErrNoSession) || (err == nil && sess.UserID != store.LocalUserID) {
		http.NotFound(w, r)
		return
	}
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

// Sessions is a list response, stamped with the server's clock so the
// dashboard computes elapsed and remaining time against it, not the phone's.
type Sessions struct {
	ServerTime time.Time `json:"server_time"`
	Sessions   []Session `json:"sessions"`
}

func (a *API) writeSessions(w http.ResponseWriter, ctx context.Context, list []*model.Session, onlyShown bool) {
	devs, err := a.Store.Devices(ctx, store.LocalUserID)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := Sessions{ServerTime: a.now(), Sessions: []Session{}}
	for _, s := range list {
		if onlyShown && s.Hidden != model.Shown {
			continue
		}
		v, err := a.view(ctx, s, devs)
		if err != nil {
			a.fail(w, err)
			return
		}
		out.Sessions = append(out.Sessions, v)
	}
	writeJSON(w, out)
}

func (a *API) fail(w http.ResponseWriter, err error) {
	if a.Log != nil {
		a.Log.Error("api", "err", err)
	}
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		fmt.Println("api: write:", err)
	}
}
