package netflix

import (
	"context"
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

// Connect signs in to Netflix in a browser window with Couchlog's own
// profile (profileDir, kept, so reconnecting needs no new sign-in while
// Netflix still trusts it) and saves the session to sessionPath.
//
// The window is a plain browser, with nothing watching or driving it: you
// sign in, pick your profile and close it. Only then is the same profile
// opened again, without a window and without loading Netflix, to read the
// cookies it kept, through the browser itself (which is what can decrypt
// them on every system). status, if set, is told what is happening.
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
	win := exec.CommandContext(ctx, bin, "--user-data-dir="+profileDir, "--no-first-run",
		"--no-default-browser-check", "--new-window", "https://www.netflix.com/login")
	if err := win.Start(); err != nil {
		return nil, fmt.Errorf("netflix: opening %s: %w", filepath.Base(bin), err)
	}
	status("Sign in to Netflix in the window that opened, pick your profile, then close the window")
	if err := win.Wait(); err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}

	status("Reading the session…")
	cookies, err := readProfileCookies(ctx, bin, profileDir)
	if err != nil {
		return nil, err
	}
	if cookies["NetflixId"] == "" {
		return nil, errors.New("netflix: no Netflix sign-in in that window; sign in and pick your profile before closing it")
	}
	c := &Client{Cookies: func() (map[string]string, string, error) { return cookies, "connect", nil }}
	p, err := c.Profile(ctx)
	if err != nil {
		return nil, fmt.Errorf("netflix: signed in, but no profile chosen yet (%w)", err)
	}
	if err := (&Saved{Cookies: cookies, SavedAt: time.Now(), How: "connect"}).Save(sessionPath); err != nil {
		return nil, err
	}
	status("Connected as " + p.Name)
	return p, nil
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
