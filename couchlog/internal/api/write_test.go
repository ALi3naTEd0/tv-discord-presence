package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/identify"
	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/store"
)

type fakeCatalog struct{}

func (fakeCatalog) Search(ctx context.Context, q string) ([]identify.Title, error) {
	return []identify.Title{{Kind: identify.Show, TMDB: 277831, Title: "The Yogurt Shop Murders", Year: 2025}}, nil
}
func (fakeCatalog) Get(ctx context.Context, k identify.Kind, id int) (identify.Title, error) {
	if k == identify.Movie {
		return identify.Title{Kind: k, TMDB: id, Title: "A Film", Year: 2024, Runtime: 100, IMDb: "tt9"}, nil
	}
	return identify.Title{Kind: k, TMDB: id, Title: "The Yogurt Shop Murders", Year: 2025,
		Seasons: []identify.Season{{Number: 1, Episodes: 5}}}, nil
}
func (fakeCatalog) Season(ctx context.Context, show, season int) ([]identify.Episode, error) {
	var eps []identify.Episode
	for i, n := range []string{"Fire and Water", "The Fifth Victim", "Mental Evidence", "In Your Time", "The End of Wondering"} {
		eps = append(eps, identify.Episode{Number: i + 1, Name: n, Runtime: 58})
	}
	return eps, nil
}
func (fakeCatalog) Find(ctx context.Context, id, source string) ([]identify.Title, error) {
	return nil, nil
}

func rig(t *testing.T) (*API, *store.Store, *model.Session) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	dev, _ := st.EnsureDevice(ctx, store.LocalUserID, "androidtv", "Box", "")
	sess, _ := st.StartSession(ctx, dev, model.App{ID: "com.wbd.stream", Name: "HBO Max"}, "", now.Add(-time.Hour))
	st.SetHint(ctx, sess.ID, model.Hint{Title: "In Your Own Time", Subtitle: "The Yogurt Shop Murders"})
	st.Touch(ctx, sess.ID, now.Add(-time.Minute), 58*time.Minute, true)
	st.CloseSession(ctx, sess.ID, time.Minute, 0)
	a := &API{Store: st, Ident: &identify.Identifier{Catalog: fakeCatalog{}, Aliases: st}, Sinks: []string{"trakt"},
		Now: func() time.Time { return now }}
	return a, st, sess
}

func do(a *API, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	return rec
}

func TestIdentifyRemembersAndCounts(t *testing.T) {
	a, st, sess := rig(t)
	ctx := context.Background()
	rec := do(a, "POST", "/api/v1/sessions/1/identify", `{"kind":"tv","tmdb":277831,"season":1,"episodes":[4],"remember":true}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var v Session
	json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Items) != 1 || v.Items[0].EpisodeTitle != "In Your Time" || v.Items[0].Deliveries["trakt"].Status != "pending" {
		t.Errorf("%+v", v.Items)
	}
	al, _ := st.FindAlias(ctx, store.LocalUserID, "com.wbd.stream", "The Yogurt Shop Murders", 0)
	if al == nil || al.TMDB != 277831 {
		t.Errorf("alias %+v", al)
	}
	// the season picker marks it watched
	rec = do(a, "GET", "/api/v1/shows/277831/seasons/1", "")
	var eps []Episode
	json.Unmarshal(rec.Body.Bytes(), &eps)
	if len(eps) != 5 || !eps[3].Watched || eps[0].Watched {
		t.Errorf("%+v", eps)
	}
	// insights count it, up next is E5
	rec = do(a, "GET", "/api/v1/insights?days=7", "")
	var in Insights
	json.Unmarshal(rec.Body.Bytes(), &in)
	if in.Episodes.N != 1 || in.Sessions != 1 || len(in.Services) != 1 || in.Services[0].Name != "HBO Max" || len(in.TopShows) != 1 {
		t.Errorf("%+v", in)
	}
	rec = do(a, "GET", "/api/v1/upnext", "")
	var up []UpNext
	json.Unmarshal(rec.Body.Bytes(), &up)
	if len(up) != 1 || up[0].Episode != 5 || up[0].Service.Key != "hbomax" {
		t.Errorf("up next %+v", up)
	}
	_ = sess
}

func TestWritesNeedJSON(t *testing.T) {
	a, _, _ := rig(t)
	req := httptest.NewRequest("POST", "/api/v1/sessions/1/hidden", strings.NewReader("hidden=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Errorf("form post: %d", rec.Code)
	}
	if rec := do(a, "POST", "/api/v1/sessions/1/hidden", `{"hidden":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hidden":"ignored"`) {
		t.Errorf("hide: %d %s", rec.Code, rec.Body)
	}
}

func TestServicePrices(t *testing.T) {
	a, _, _ := rig(t)
	rec := do(a, "PUT", "/api/v1/services/hbomax", `{"price":149,"currency":"mxn","period":"month"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var s Services
	json.Unmarshal(rec.Body.Bytes(), &s)
	if len(s.Services) != 1 || s.Services[0].Price == nil || s.Services[0].Price.Currency != "MXN" || s.Services[0].PerHour == nil {
		t.Fatalf("%+v", s)
	}
	if d := s.Services[0].Devices; len(d) != 1 || d[0] != "Box" {
		t.Errorf("devices %v", d)
	}
	if rec := do(a, "PUT", "/api/v1/settings/currency", `{"value":"usd"}`); rec.Code != 400 {
		t.Errorf("lower-case currency: %d", rec.Code)
	}
	if rec := do(a, "PUT", "/api/v1/settings/currency", `{"value":"USD"}`); rec.Code != 204 {
		t.Errorf("currency: %d", rec.Code)
	}
}

func TestServiceProfiles(t *testing.T) {
	a, _, _ := rig(t)
	a.NetflixProfile = func(context.Context) (string, error) { return "Gordo", nil }
	if rec := do(a, "PUT", "/api/v1/settings/profile:hbomax", `{"value":"  Gordo  "}`); rec.Code != 204 {
		t.Fatalf("set: %d %s", rec.Code, rec.Body)
	}
	var s Services
	json.Unmarshal(do(a, "GET", "/api/v1/services", "").Body.Bytes(), &s)
	if len(s.Services) != 1 || s.Services[0].Profile != "Gordo" || s.Services[0].ProfileFrom != "you" {
		t.Errorf("%+v", s.Services)
	}
	if rec := do(a, "PUT", "/api/v1/settings/profile:hbomax", `{"value":"`+strings.Repeat("x", 61)+`"}`); rec.Code != 400 {
		t.Errorf("long name: %d", rec.Code)
	}
}
