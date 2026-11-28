// Package config reads Couchlog's settings file (TOML). Secrets never live
// in it: the TMDB key and the Plex token come from the environment
// (COUCHLOG_TMDB_KEY / COUCHLOG_PLEX_TOKEN, or tvpresence's TMDB_API_KEY /
// PLEX_TOKEN), so the file can be shared or committed.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Device is one TV or box to listen to.
type Device struct {
	Kind    string `toml:"kind"`     // webos | androidtv
	Name    string `toml:"name"`     // shown in the dashboard; also the device's identity in the database
	Host    string `toml:"host"`     // IP or name; androidtv may add :port
	KeyFile string `toml:"key_file"` // pairing key (webOS client key, adb private key)
}

// Plex is the Plex server to import from.
type Plex struct {
	URL        string `toml:"url"`
	Account    int    `toml:"account"`     // 1 = the server owner
	User       string `toml:"user"`        // optional account name (shared servers)
	DB         string `toml:"db"`          // Plex's library database; "" = the OS default when readable
	ImportDays int    `toml:"import_days"` // look-back on the first import
}

// Config is the whole file.
type Config struct {
	DB       string   `toml:"db"` // Couchlog's database; "" = the default
	Devices  []Device `toml:"device"`
	Plex     *Plex    `toml:"plex"`
	Language string   `toml:"language"` // TMDB language, e.g. "es-MX"
	// Region is your country (ISO 3166-1, "MX"), for where titles stream;
	// "" = the country in Language ("es-MX" → MX), else the time zone's.
	Region string `toml:"region"`
	// Sinks are the services identified titles are sent to ("trakt",
	// "simkl"). Empty: Couchlog only records (the parallel run).
	Sinks []string `toml:"sinks"`
	// EnvFiles are extra KEY=value files read for secrets when the
	// environment does not have them (tvpresence's .env works).
	EnvFiles []string `toml:"env_files"`
	// Netflix identifies Netflix sessions from your account's viewing
	// activity, with the Netflix session of a browser on this computer.
	Netflix bool `toml:"netflix"`
	// ReadOnly: destinations are only checked, never written to (the
	// parallel run, while tvpresence still sends).
	ReadOnly bool `toml:"read_only"`
	// Trakt and Simkl credentials, until Couchlog's own sign-in: for now
	// trakt-scrobbler's token and your Simkl folder. The client IDs are
	// public API keys; the tokens stay in their files.
	Trakt *Service `toml:"trakt"`
	Simkl *Service `toml:"simkl"`
	// Presence shows what you watch as your Discord status: "on", "off"
	// (default), or "dry-run" (log it only, while tvpresence owns it).
	Presence string `toml:"presence"`
	// DiscordClientID is the Discord application shown ("Watching …");
	// "" = Couchlog's own, built in.
	DiscordClientID string `toml:"discord_client_id"`
	// Listen is where the dashboard and its API are served; "" =
	// 127.0.0.1:7791 (this computer only), "off" = not served.
	Listen string `toml:"listen"`
}

// DefaultListen is the dashboard's address when the settings name none.
const DefaultListen = "127.0.0.1:7791"

// Service is a destination's credentials.
type Service struct {
	ClientID  string `toml:"client_id"`  // "" = COUCHLOG_<NAME>_CLIENT_ID, or SIMKL_CLIENT_ID in env files
	TokenFile string `toml:"token_file"` // JSON with access_token (and expires_at / created_at+expires_in)
	// RenewCommand is run when the token has expired, so the app that owns
	// it renews it (a refresh token is single-use; only one app may spend it).
	// Empty: "trakts auth" when the token belongs to trakt-scrobbler.
	RenewCommand []string `toml:"renew_command"`
}

// Load reads a settings file.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for i := range c.Devices {
		d := &c.Devices[i]
		d.KeyFile = Expand(d.KeyFile)
		if d.Kind != "webos" && d.Kind != "androidtv" {
			return nil, fmt.Errorf("config: device %q: kind must be webos or androidtv, not %q", d.Name, d.Kind)
		}
		if d.Host == "" || d.Name == "" {
			return nil, fmt.Errorf("config: every device needs a name and a host")
		}
	}
	for i, f := range c.EnvFiles {
		c.EnvFiles[i] = Expand(f)
	}
	c.DB = Expand(c.DB)
	for _, s := range []*Service{c.Trakt, c.Simkl} {
		if s != nil {
			s.TokenFile = Expand(s.TokenFile)
		}
	}
	if c.Plex != nil {
		c.Plex.DB = Expand(c.Plex.DB)
		if c.Plex.Account == 0 {
			c.Plex.Account = 1
		}
		if c.Plex.ImportDays == 0 {
			c.Plex.ImportDays = 7
		}
	}
	return &c, nil
}

// Save writes a settings file, creating its folder.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintln(f, "# Couchlog settings. Secrets (TMDB key, Plex token) come from the environment or env_files.")
	return toml.NewEncoder(f).Encode(c)
}

// Expand turns a leading ~ into the home folder.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Secret returns the first of names set in the environment, else in the
// env files.
func (c *Config) Secret(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	for _, f := range c.EnvFiles {
		vals := readEnv(f)
		for _, n := range names {
			if v := vals[n]; v != "" {
				return v
			}
		}
	}
	return ""
}

func readEnv(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

// FromTvpresence builds Couchlog settings from a tvpresence config.toml:
// the same TV, box and keys (no new pairing), Plex on this machine, and
// tvpresence's env files for the secrets. Nothing is sent anywhere (no
// sinks) until you add them.
func FromTvpresence(path string) (*Config, error) {
	var old struct {
		TVHost    string `toml:"tv_host"`
		ClientID  string `toml:"client_id"`
		AndroidTV struct {
			Host string `toml:"host"`
			Port int    `toml:"port"`
		} `toml:"androidtv"`
		History struct {
			EnvFiles []string `toml:"env_files"`
			PlexURL  string   `toml:"plex_url"`
			PlexAcct int      `toml:"plex_account_id"`
			PlexUser string   `toml:"plex_user"`
			PlexDB   string   `toml:"plex_db"`
			PlexDays int      `toml:"plex_import_days"`
			TMDBLang string   `toml:"tmdb_language"`
			Plex     *bool    `toml:"plex"`
		} `toml:"history"`
	}
	if _, err := toml.DecodeFile(path, &old); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	dir := filepath.Dir(path)
	c := &Config{EnvFiles: old.History.EnvFiles, Language: old.History.TMDBLang}
	if old.TVHost != "" {
		c.Devices = append(c.Devices, Device{Kind: "webos", Name: "LG TV", Host: old.TVHost, KeyFile: filepath.Join(dir, "key")})
	}
	if h := old.AndroidTV.Host; h != "" {
		if old.AndroidTV.Port != 0 && old.AndroidTV.Port != 5555 {
			h = fmt.Sprintf("%s:%d", h, old.AndroidTV.Port)
		}
		c.Devices = append(c.Devices, Device{Kind: "androidtv", Name: "Android TV", Host: h, KeyFile: filepath.Join(dir, "adbkey")})
	}
	c.EnvFiles = append(c.EnvFiles, filepath.Join(dir, ".env"))
	url := old.History.PlexURL
	if url == "" {
		url = "http://127.0.0.1:32400"
	}
	c.Plex = &Plex{URL: url, Account: max(old.History.PlexAcct, 1), User: old.History.PlexUser,
		DB: old.History.PlexDB, ImportDays: max(old.History.PlexDays, 7)}
	if len(c.Devices) == 0 {
		return nil, errors.New("config: no TV or box in " + path)
	}
	// The parallel run: check against Trakt and Simkl, never write
	// (tvpresence still sends), with the credentials it already uses.
	home, _ := os.UserHomeDir()
	c.ReadOnly = true
	c.Presence, c.DiscordClientID = "dry-run", old.ClientID // tvpresence still shows your status
	c.Sinks = []string{"trakt", "simkl"}
	c.Trakt = &Service{TokenFile: filepath.Join(home, ".local", "share", "trakt-scrobbler", "trakt_token.json")}
	c.Simkl = &Service{TokenFile: filepath.Join(home, ".config", "simkl", "token.json")}
	c.EnvFiles = append(c.EnvFiles, filepath.Join(home, ".config", "simkl", ".env"))
	return c, nil
}

// Country is Region, or the country part of Language ("es-MX" → "MX").
func (c *Config) Country() string {
	if c.Region != "" {
		return strings.ToUpper(c.Region)
	}
	if i := strings.LastIndex(c.Language, "-"); i > 0 && len(c.Language)-i == 3 {
		return strings.ToUpper(c.Language[i+1:])
	}
	// the computer's time zone: America/Merida → MX (the language is a poor
	// guide: many systems in Mexico run in English)
	return zoneCountry()
}

// zoneCountry is the country of the computer's time zone, from the system's
// zone.tab (Linux, macOS); "" when it cannot tell.
func zoneCountry() string {
	zone := os.Getenv("TZ")
	if zone == "" {
		if l, err := os.Readlink("/etc/localtime"); err == nil {
			if i := strings.Index(l, "zoneinfo/"); i >= 0 {
				zone = l[i+len("zoneinfo/"):]
			}
		}
	}
	zone = strings.TrimPrefix(zone, ":")
	if zone == "" {
		return ""
	}
	for _, tab := range []string{"/usr/share/zoneinfo/zone.tab", "/usr/share/zoneinfo/zone1970.tab"} {
		b, err := os.ReadFile(tab)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Split(line, "\t")
			if len(f) >= 3 && f[2] == zone && len(f[0]) == 2 {
				return f[0]
			}
		}
	}
	return ""
}
