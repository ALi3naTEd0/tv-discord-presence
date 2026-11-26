// Package netflix identifies what you watched on Netflix. The Netflix app
// for TVs runs its own engine and tells the TV nothing (no title in its
// media session, no logs, no Watch Next row), but your account keeps the
// viewing activity with the exact start time of each title. Couchlog reads
// it with your Netflix session through Netflix's internal web API, as
// Universal Trakt Scrobbler does (MIT, github.com/trakt-tools/
// universal-trakt-scrobbler); it is not public and can move.
//
// The session comes from "Connect Netflix" (Couchlog's own browser window,
// see Connect) or, with nothing to do at all, from a browser on this
// machine that is already signed in (BrowserCookies). It is only ever sent
// to Netflix.
package netflix

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNoSession means no browser here is signed in to Netflix.
var ErrNoSession = errors.New("netflix: not signed in to Netflix in a readable browser on this computer")

// firefoxRoots are where Firefox-family browsers keep their profiles. Their
// cookie store is a plain SQLite file; Chromium encrypts its own (and on
// Windows, Chrome and Edge make it unreadable to other programs).
func firefoxRoots() []string {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	roots := []string{
		filepath.Join(xdg, "zen"), filepath.Join(xdg, "mozilla", "firefox"),
		filepath.Join(home, ".zen"), filepath.Join(home, ".mozilla", "firefox"),
		filepath.Join(home, ".librewolf"), filepath.Join(home, ".floorp"),
		filepath.Join(home, ".var", "app", "app.zen_browser.zen", ".zen"),
		filepath.Join(home, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
		filepath.Join(home, "snap", "firefox", "common", ".mozilla", "firefox"),
	}
	switch runtime.GOOS {
	case "darwin":
		as := filepath.Join(home, "Library", "Application Support")
		roots = append(roots, filepath.Join(as, "zen", "Profiles"), filepath.Join(as, "Firefox", "Profiles"))
	case "windows":
		if ad := os.Getenv("APPDATA"); ad != "" {
			roots = append(roots, filepath.Join(ad, "zen", "Profiles"), filepath.Join(ad, "Mozilla", "Firefox", "Profiles"))
		}
	}
	return roots
}

// CookieStores lists the cookies.sqlite of every Firefox-family profile.
func CookieStores() []string {
	var out []string
	for _, r := range firefoxRoots() {
		m, _ := filepath.Glob(filepath.Join(r, "*", "cookies.sqlite"))
		out = append(out, m...)
	}
	return out
}

// BrowserCookies returns the netflix.com cookies of the profile that used
// Netflix most recently, and which profile that was. The browser keeps its
// database locked, so a snapshot (with its WAL) is read.
func BrowserCookies() (map[string]string, string, error) {
	return cookiesFrom(CookieStores())
}

func cookiesFrom(stores []string) (map[string]string, string, error) {
	var best map[string]string
	var bestAt int64
	var src string
	for _, path := range stores {
		cookies, at, err := readStore(path)
		if err != nil || cookies["NetflixId"] == "" {
			continue
		}
		if at > bestAt || best == nil {
			best, bestAt, src = cookies, at, filepath.Dir(path)
		}
	}
	if best == nil {
		return nil, "", ErrNoSession
	}
	return best, src, nil
}

func readStore(path string) (map[string]string, int64, error) {
	tmp, err := os.MkdirTemp("", "couchlog-cookies-")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "cookies.sqlite")
	if err := copyFile(path, dst); err != nil {
		return nil, 0, err
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + ext); err == nil {
			copyFile(path+ext, dst+ext)
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dst))
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, value, lastAccessed FROM moz_cookies
		WHERE host LIKE '%netflix.com' AND (expiry = 0 OR expiry > ?)`, time.Now().Unix())
	if err != nil {
		return nil, 0, fmt.Errorf("netflix: %s: %w", path, err)
	}
	defer rows.Close()
	out := map[string]string{}
	var at int64
	for rows.Next() {
		var name, value string
		var accessed int64
		if rows.Scan(&name, &value, &accessed) != nil {
			continue
		}
		out[name] = value
		if name == "NetflixId" && accessed > at {
			at = accessed
		}
	}
	return out, at, rows.Err()
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func cookieHeader(c map[string]string) string {
	parts := make([]string, 0, len(c))
	for k, v := range c {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}
