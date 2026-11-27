package api

import (
	"context"
	"net/http"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/discover"
)

// DeviceHooks are what the setup screen needs from the running server;
// any may be nil (tests, or a server without devices).
type DeviceHooks struct {
	States         func() map[int64]DeviceState
	Scan           func(ctx context.Context) ([]FoundDevice, error)
	Add            func(ctx context.Context, kind, host, name string) (int64, error)
	ConnectNetflix func() error
	NetflixStatus  func(ctx context.Context) NetflixStatus
}

// DeviceState is what a device is doing now.
type DeviceState struct {
	// State: playing | paused | idle | off, or "waiting" before the first
	// answer (a new TV waiting for you to accept the connection on it).
	State string    `json:"state"`
	Since time.Time `json:"since,omitzero"`
	App   string    `json:"app,omitempty"`
}

// FoundDevice is a TV or box seen on the network.
type FoundDevice struct {
	discover.Found
	AddedAs string `json:"added_as,omitempty"` // the name it has in Couchlog, when already added
}

// NetflixStatus is the Netflix connection.
type NetflixStatus struct {
	Enabled    bool       `json:"enabled"` // Netflix identification is on in the settings
	Saved      bool       `json:"saved"`   // a session is saved
	SavedAt    *time.Time `json:"saved_at,omitempty"`
	Profile    string     `json:"profile,omitempty"` // read with the saved session: it works
	Connecting bool       `json:"connecting"`
	Message    string     `json:"message,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func (a *API) scan(w http.ResponseWriter, r *http.Request) {
	if a.Devices.Scan == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	found, err := a.Devices.Scan(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, found)
}

func (a *API) addDevice(w http.ResponseWriter, r *http.Request) {
	if a.Devices.Add == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Kind string `json:"kind"`
		Host string `json:"host"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id, err := a.Devices.Add(r.Context(), req.Kind, req.Host, req.Name)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"id": id})
}

func (a *API) connectNetflix(w http.ResponseWriter, r *http.Request) {
	if a.Devices.ConnectNetflix == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	var req struct{}
	if !readJSON(w, r, &req) {
		return
	}
	if err := a.Devices.ConnectNetflix(); err != nil {
		a.fail(w, err)
		return
	}
	a.netflixStatus(w, r)
}

func (a *API) netflixStatus(w http.ResponseWriter, r *http.Request) {
	if a.Devices.NetflixStatus == nil {
		writeJSON(w, NetflixStatus{})
		return
	}
	writeJSON(w, a.Devices.NetflixStatus(r.Context()))
}
