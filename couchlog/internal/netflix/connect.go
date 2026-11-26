package netflix

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Saved is the Netflix session Couchlog owns: taken from its own browser
// window ("Connect Netflix") or pasted. It lives in a 0600 file next to the
// database and is only ever sent to Netflix.
type Saved struct {
	Cookies map[string]string `json:"cookies"`
	SavedAt time.Time         `json:"saved_at"`
	How     string            `json:"how"` // connect | paste
}

// SessionPath is where the saved session lives, in Couchlog's folder.
func SessionPath(dir string) string { return filepath.Join(dir, "netflix-session.json") }

// LoadSaved reads a saved session; os.ErrNotExist when there is none.
func LoadSaved(path string) (*Saved, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Saved
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("netflix: %s: %w", path, err)
	}
	if s.Cookies["NetflixId"] == "" {
		return nil, os.ErrNotExist
	}
	return &s, nil
}

// Save writes the session atomically, readable only by you.
func (s *Saved) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SessionCookies is the cookie source Couchlog uses: its own saved session
// first, else a readable browser on this machine.
func SessionCookies(path string) func() (map[string]string, string, error) {
	return func() (map[string]string, string, error) {
		if s, err := LoadSaved(path); err == nil {
			return s.Cookies, "couchlog (" + s.How + ")", nil
		}
		return BrowserCookies()
	}
}

// ParseCookieHeader reads "NetflixId=...; SecureNetflixId=..." as pasted
// from a browser's developer tools.
func ParseCookieHeader(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// Chromium-family browsers, in order of preference; Edge comes with every
// Windows. Their debugging protocol (CDP) hands over the window's cookies,
// HttpOnly included.
func chromiumCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		var out []string
		for _, env := range []string{"PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			out = append(out,
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(base, "BraveSoftware", "Brave-Browser", "Application", "brave.exe"))
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	}
	return []string{"google-chrome-stable", "google-chrome", "chromium", "chromium-browser",
		"microsoft-edge-stable", "microsoft-edge", "brave", "brave-browser", "vivaldi-stable"}
}

// ErrNoBrowser means no Chromium-family browser is installed.
var ErrNoBrowser = errors.New("netflix: no Chrome, Edge, Brave or Chromium found to open the sign-in window")

func findChromium() (string, error) {
	for _, c := range chromiumCandidates() {
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
	}
	return "", ErrNoBrowser
}

// Connect signs in to Netflix with Couchlog's own browser profile
// (profileDir, kept between connections) and saves the session to
// sessionPath, asking as little as possible of you:
//
//  1. If that profile is still signed in, it connects without a window.
//  2. Otherwise a plain window opens on Netflix's sign-in page, with nothing
//     watching or driving the page. Chrome keeps cookie names unencrypted
//     on disk, so Couchlog sees when the sign-in and the profile choice
//     have happened, closes the window itself and reads the session.
//  3. If you close the window first, it reads the session right then.
//
// The session is read by opening the same profile again with no window
// and no page and asking the browser itself, which is what can decrypt
// its cookies on every system. status, if set, is told what is happening.
func Connect(ctx context.Context, profileDir, sessionPath string, status func(string)) (*Profile, error) {
	if status == nil {
		status = func(string) {}
	}
	bin, err := findChromium()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, err
	}
	if p, err := takeSession(ctx, bin, profileDir, sessionPath); err == nil {
		status("Connected as " + p.Name)
		return p, nil
	}
	page, ask := "https://www.netflix.com/login",
		"Sign in to Netflix in the window that opened and pick your profile; it closes by itself (up to half a minute)"
	for round := 0; round < 2; round++ {
		start := time.Now()
		win := exec.Command(bin, "--user-data-dir="+profileDir, "--no-first-run", "--no-default-browser-check",
			"--new-window", page)
		if err := win.Start(); err != nil {
			return nil, fmt.Errorf("netflix: opening %s: %w", filepath.Base(bin), err)
		}
		closed := make(chan struct{})
		go func() { win.Wait(); close(closed) }()
		status(ask)
		select {
		case <-closed:
		case <-ctx.Done():
			win.Process.Kill()
			return nil, ctx.Err()
		case <-signedIn(ctx, profileDir, start, closed):
			closeWindow(win)
			select {
			case <-closed:
			case <-time.After(15 * time.Second):
				win.Process.Kill()
				<-closed
			}
		}
		status("Reading the session…")
		p, err := takeSession(ctx, bin, profileDir, sessionPath)
		switch {
		case err == nil:
			status("Connected as " + p.Name)
			return p, nil
		case errors.Is(err, errNotSignedIn):
			return nil, errors.New("netflix: the window closed before signing in to Netflix; try again")
		}
		// Signed in, no profile yet: once more, on the profile picker.
		page, ask = "https://www.netflix.com/browse", "Pick your Netflix profile in the window; it closes by itself"
	}
	return nil, errors.New("netflix: signed in, but Netflix did not open a profile; try again and pick one")
}

var errNotSignedIn = errors.New("netflix: not signed in")

// takeSession reads the profile's session and keeps it if Netflix opens a
// profile with it.
func takeSession(ctx context.Context, bin, profileDir, sessionPath string) (*Profile, error) {
	cookies, err := readProfileCookies(ctx, bin, profileDir)
	if err != nil {
		return nil, err
	}
	if cookies["NetflixId"] == "" {
		return nil, errNotSignedIn
	}
	c := &Client{Cookies: func() (map[string]string, string, error) { return cookies, "connect", nil }}
	p, err := c.Profile(ctx)
	if err != nil {
		return nil, err
	}
	if err := (&Saved{Cookies: cookies, SavedAt: time.Now(), How: "connect"}).Save(sessionPath); err != nil {
		return nil, err
	}
	return p, nil
}

// signedIn reports (by closing the channel) when the profile's cookie
// store shows a sign-in made after start and the profile choice that
// follows it: Netflix sets NetflixId when you sign in and profilesNewSession
// right after, at the profile choice. Only cookie names and times are read,
// which Chrome keeps unencrypted; nothing touches the page. Where the store
// cannot be read (Windows keeps it locked), it never fires, and closing the
// window is what continues.
func signedIn(ctx context.Context, profileDir string, start time.Time, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(1500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-tick.C:
			}
			c, err := cookieTimes(profileDir)
			if err != nil {
				continue
			}
			id, ok := c["NetflixId"]
			ps, ok2 := c["profilesNewSession"]
			if ok && ok2 && (id.created.After(start) || ps.created.After(start)) && !ps.accessed.Before(id.created) {
				// Chrome writes cookies to disk in batches (about every 30 s),
				// so by the time they show the profile choice is long done.
				close(done)
				return
			}
		}
	}()
	return done
}

type cookieTime struct{ created, accessed time.Time }

// cookieTimes reads the names and times of the profile's netflix.com
// cookies from a snapshot of Chrome's cookie store.
func cookieTimes(profileDir string) (map[string]cookieTime, error) {
	var src string
	for _, p := range []string{filepath.Join(profileDir, "Default", "Network", "Cookies"), filepath.Join(profileDir, "Default", "Cookies")} {
		if _, err := os.Stat(p); err == nil {
			src = p
			break
		}
	}
	if src == "" {
		return nil, os.ErrNotExist
	}
	tmp, err := os.MkdirTemp("", "couchlog-cookie-names-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "Cookies")
	if err := copyFile(src, dst); err != nil {
		return nil, err
	}
	if _, err := os.Stat(src + "-journal"); err == nil {
		copyFile(src+"-journal", dst+"-journal")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dst))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT name, creation_utc, last_access_utc FROM cookies WHERE host_key LIKE '%netflix.com'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]cookieTime{}
	for rows.Next() {
		var name string
		var created, accessed int64
		if rows.Scan(&name, &created, &accessed) == nil {
			out[name] = cookieTime{chromeTime(created), chromeTime(accessed)}
		}
	}
	return out, rows.Err()
}

// chromeTime converts Chrome's microseconds since 1601 to a time.
func chromeTime(us int64) time.Time {
	const epochDiff = 11644473600 // seconds from 1601-01-01 to 1970-01-01
	return time.Unix(us/1e6-epochDiff, (us%1e6)*1e3)
}

// readProfileCookies opens the profile with no window and no page, asks
// the browser for its netflix.com cookies over its debugging protocol and
// closes it.
func readProfileCookies(ctx context.Context, bin, profileDir string) (map[string]string, error) {
	portFile := filepath.Join(profileDir, "DevToolsActivePort")
	os.Remove(portFile)
	cmd := exec.Command(bin, "--headless=new", "--user-data-dir="+profileDir, "--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1", "--no-first-run", "--no-default-browser-check", "about:blank")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	ws, err := debuggerURL(ctx, portFile)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, ws, nil)
	if err != nil {
		return nil, fmt.Errorf("netflix: reading the session: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)
	if err := wsjson.Write(ctx, conn, map[string]any{"id": 1, "method": "Storage.getCookies"}); err != nil {
		return nil, err
	}
	for {
		var m struct {
			ID     int `json:"id"`
			Result struct {
				Cookies []struct {
					Name   string `json:"name"`
					Value  string `json:"value"`
					Domain string `json:"domain"`
				} `json:"cookies"`
			} `json:"result"`
		}
		if err := wsjson.Read(ctx, conn, &m); err != nil {
			return nil, fmt.Errorf("netflix: reading the session: %w", err)
		}
		if m.ID != 1 {
			continue
		}
		out := map[string]string{}
		for _, c := range m.Result.Cookies {
			if strings.HasSuffix(c.Domain, "netflix.com") {
				out[c.Name] = c.Value
			}
		}
		wsjson.Write(ctx, conn, map[string]any{"id": 2, "method": "Browser.close"})
		return out, nil
	}
}

// debuggerURL waits for the browser to write its debugging port.
func debuggerURL(ctx context.Context, portFile string) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(portFile); err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) == 2 {
				return "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1]), nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return "", errors.New("netflix: the browser did not open its debugging port")
}
