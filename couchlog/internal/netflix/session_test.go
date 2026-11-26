package netflix

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSavedSessionComesFirst(t *testing.T) {
	path := SessionPath(t.TempDir())
	get := SessionCookies(path)
	// Nothing saved: falls back to the browsers (none in a test home).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, _, err := get(); err != ErrNoSession {
		t.Errorf("err %v, want ErrNoSession", err)
	}
	if err := (&Saved{Cookies: map[string]string{"NetflixId": "n1"}, SavedAt: time.Now(), How: "connect"}).Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("session file mode %v", fi.Mode().Perm())
	}
	c, src, err := get()
	if err != nil || c["NetflixId"] != "n1" || src != "couchlog (connect)" {
		t.Errorf("%v %q %v", c, src, err)
	}
}

func TestParseCookieHeader(t *testing.T) {
	c := ParseCookieHeader(" NetflixId=a%3Db; SecureNetflixId=v%3D2 ;nfvdid=x ; bad")
	if len(c) != 3 || c["NetflixId"] != "a%3Db" || c["SecureNetflixId"] != "v%3D2" {
		t.Errorf("%v", c)
	}
}

func TestRenewedCookiesAreKept(t *testing.T) {
	path := SessionPath(t.TempDir())
	(&Saved{Cookies: map[string]string{"NetflixId": "old", "SecureNetflixId": "s"}, How: "connect"}).Save(path)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "NetflixId", Value: "new", Domain: "netflix.com"})
		w.Write([]byte(`{"jsonGraph":{}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Cookies: SessionCookies(path), SessionPath: path}
	if _, err := c.do(context.Background(), http.MethodGet, srv.URL+"/x", "", nil); err != nil {
		t.Fatal(err)
	}
	s, _ := LoadSaved(path)
	if s.Cookies["NetflixId"] != "new" || s.Cookies["SecureNetflixId"] != "s" {
		t.Errorf("after renewal %v", s.Cookies)
	}
	// A browser's cookies are the browser's: never written to the session file.
	other := SessionPath(t.TempDir())
	c2 := &Client{Cookies: func() (map[string]string, string, error) {
		return map[string]string{"NetflixId": "b"}, "/zen/profile", nil
	}, SessionPath: other}
	c2.do(context.Background(), http.MethodGet, srv.URL+"/x", "", nil)
	if _, err := os.Stat(other); err == nil {
		t.Error("browser cookies written as Couchlog's session")
	}
	_ = filepath.Join
}

func TestProfilePageVariant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"auiUserContext":{"data":{"name":"Gordo","guid":"G0","userGuid":"G1","x":1}},"authURL":"a"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Cookies: func() (map[string]string, string, error) { return map[string]string{"NetflixId": "x"}, "t", nil }}
	p, err := c.Profile(context.Background())
	if err != nil || p.GUID != "G1" || p.Name != "Gordo" {
		t.Errorf("%+v %v", p, err)
	}
}
