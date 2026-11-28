package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFromTvpresenceAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "config.toml")
	os.WriteFile(old, []byte(`tv_host = "192.168.1.220"
client_id = "123"
[history]
env_files = ["/x/plex/.env"]
[androidtv]
host = "192.168.1.117"
`), 0o600)
	c, err := FromTvpresence(old)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Devices) != 2 || c.Devices[0].Kind != "webos" || c.Devices[0].KeyFile != filepath.Join(dir, "key") ||
		c.Devices[1].Host != "192.168.1.117" || c.Devices[1].KeyFile != filepath.Join(dir, "adbkey") {
		t.Fatalf("devices %+v", c.Devices)
	}
	// The parallel run checks Trakt and Simkl but never writes to them.
	if c.Plex == nil || c.Plex.URL != "http://127.0.0.1:32400" || c.Plex.Account != 1 || !c.ReadOnly || len(c.Sinks) != 2 ||
		c.Trakt == nil || c.Simkl == nil {
		t.Errorf("plex %+v sinks %v", c.Plex, c.Sinks)
	}
	p := filepath.Join(dir, "couchlog", "config.toml")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Devices) != 2 || back.Devices[1].Name != "Android TV" || back.Plex.ImportDays != 7 {
		t.Errorf("round trip %+v", back)
	}
}

func TestSecret(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("# keys\nTMDB_API_KEY=\"abc\"\nexport PLEX_TOKEN=tok\n"), 0o600)
	c := &Config{EnvFiles: []string{filepath.Join(dir, "missing"), env}}
	t.Setenv("COUCHLOG_TMDB_KEY", "")
	if got := c.Secret("COUCHLOG_TMDB_KEY", "TMDB_API_KEY"); got != "abc" {
		t.Errorf("from env file: %q", got)
	}
	t.Setenv("COUCHLOG_TMDB_KEY", "fromenv")
	if got := c.Secret("COUCHLOG_TMDB_KEY", "TMDB_API_KEY"); got != "fromenv" {
		t.Errorf("environment first: %q", got)
	}
	if got := c.Secret("PLEX_TOKEN"); got != "tok" {
		t.Errorf("export line: %q", got)
	}
}

func TestLoadRejectsBadDevice(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte("[[device]]\nkind = \"roku\"\nname = \"x\"\nhost = \"y\"\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestCountry(t *testing.T) {
	t.Setenv("TZ", "America/Merida")
	if _, err := os.Stat("/usr/share/zoneinfo/zone.tab"); err == nil {
		if c := (&Config{}).Country(); c != "MX" {
			t.Errorf("from the time zone: %q", c)
		}
	}
	if c := (&Config{Language: "pt-BR"}).Country(); c != "BR" {
		t.Errorf("from language: %q", c)
	}
	if c := (&Config{Region: "us", Language: "es-MX"}).Country(); c != "US" {
		t.Errorf("region wins: %q", c)
	}
}
