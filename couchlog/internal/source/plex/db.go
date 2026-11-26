package plex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // Plex's own library database, read-only

	"github.com/ALi3naTEd0/couchlog/internal/model"
	"github.com/ALi3naTEd0/couchlog/internal/source"
)

// Plex's views table keeps each viewing's GUID even after the file is
// deleted, and Plex's metadata provider resolves that GUID to IMDb/TMDB/TVDB
// with Plex's own match (no fuzzy search). The HTTP history loses both once
// the item leaves the library, which is how most people use Plex: watch,
// then delete. So when the server's database is readable (Couchlog runs on
// the Plex machine), plays come from there.

// DefaultDBPath is where Plex keeps its library database on this OS.
func DefaultDBPath() string {
	rel := filepath.Join("Plex Media Server", "Plug-in Support", "Databases", "com.plexapp.plugins.library.db")
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, rel)
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", rel)
	}
	return filepath.Join("/var/lib/plex", rel)
}

// providerURL is Plex's metadata provider (plex:// GUIDs).
const providerURL = "https://metadata.provider.plex.tv"

type provided struct {
	md   metadata
	err  error
	when time.Time
}

func (c *Client) openDB() (*sql.DB, error) {
	if c.DBPath == "" {
		return nil, os.ErrNotExist
	}
	f, err := os.Open(c.DBPath)
	if err != nil {
		return nil, err
	}
	f.Close()
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(filepath.ToSlash(c.DBPath))
	db, err := sql.Open("sqlite", "file:"+esc+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

type view struct {
	id                      int64
	mtype                   int // 1 movie, 4 episode
	guid, grandparentGUID   string
	title, grandparentTitle string
	season, episode         int
	viewedAt                int64
	released                any // originally_available_at: epoch or date text
}

// playsFromDB reads this account's views after since and resolves them.
func (c *Client) playsFromDB(ctx context.Context, since time.Time) ([]source.Play, error) {
	db, err := c.openDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, metadata_type, COALESCE(guid,''), COALESCE(grandparent_guid,''),
		COALESCE(title,''), COALESCE(grandparent_title,''), COALESCE(parent_index,0), COALESCE("index",0),
		viewed_at, originally_available_at
		FROM metadata_item_views WHERE account_id = ? AND viewed_at > ? AND metadata_type IN (1, 4)
		ORDER BY viewed_at`, c.AccountID, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("plex db: %w", err)
	}
	var views []view
	for rows.Next() {
		var v view
		if err := rows.Scan(&v.id, &v.mtype, &v.guid, &v.grandparentGUID, &v.title, &v.grandparentTitle,
			&v.season, &v.episode, &v.viewedAt, &v.released); err != nil {
			rows.Close()
			return nil, fmt.Errorf("plex db: %w", err)
		}
		views = append(views, v)
	}
	rows.Close()
	out := make([]source.Play, 0, len(views))
	for _, v := range views {
		out = append(out, c.playOf(ctx, db, v))
	}
	return out, nil
}

// playOf resolves one view. A GUID that does not resolve leaves the play
// unidentified (the picker, or the next import, takes it).
func (c *Client) playOf(ctx context.Context, db *sql.DB, v view) source.Play {
	viewed := time.Unix(v.viewedAt, 0)
	p := source.Play{SourceKey: fmt.Sprintf("/status/sessions/history/%d", v.id), DeviceID: c.DeviceID,
		StartedAt: viewed, EndedAt: viewed}
	own, _ := c.resolve(ctx, db, v.guid) // the movie or episode: its duration
	if v.mtype == 4 {
		p.Identity = model.Identity{Kind: model.KindEpisode, Title: v.grandparentTitle, Season: v.season,
			Episode: v.episode, EpisodeTitle: v.title}
		if show, err := c.resolve(ctx, db, v.grandparentGUID); err == nil {
			if show.Title != "" {
				p.Identity.Title = show.Title
			}
			p.Identity.Year = show.Year
			p.Identity.TMDB, p.Identity.TVDB, p.Identity.IMDb = show.ids()
		}
	} else {
		p.Identity = model.Identity{Kind: model.KindMovie, Title: v.title, Year: yearOfReleased(v.released)}
		if own != nil {
			if own.Title != "" {
				p.Identity.Title = own.Title
			}
			if own.Year != 0 {
				p.Identity.Year = own.Year
			}
			p.Identity.TMDB, _, p.Identity.IMDb = own.ids()
		}
	}
	// The length comes from the provider even when it lists no IDs for the
	// item itself (episodes usually do not; the show has them).
	var dur int64
	if own != nil {
		dur = own.Duration
	}
	if dur == 0 && strings.HasPrefix(v.guid, "plex://") {
		if m, err := c.provider(ctx, v.guid); err == nil {
			dur = m.Duration
		}
	}
	p.StartedAt = viewed.Add(-time.Duration(dur) * time.Millisecond)
	return p
}

func yearOfReleased(v any) int {
	switch x := v.(type) {
	case int64:
		if x > 0 {
			return time.Unix(x, 0).UTC().Year()
		}
	case float64:
		if x > 0 {
			return time.Unix(int64(x), 0).UTC().Year()
		}
	case string:
		if len(x) >= 4 {
			y, _ := strconv.Atoi(x[:4])
			return y
		}
	case []byte:
		return yearOfReleased(string(x))
	}
	return 0
}

var errNoIDs = errors.New("plex: no external IDs")

// resolve turns a Plex GUID into metadata with external IDs: plex:// GUIDs
// through the metadata provider (falling back to the IDs the library stored,
// for titles the provider does not know yet), legacy agent GUIDs as they are.
func (c *Client) resolve(ctx context.Context, db *sql.DB, guid string) (*metadata, error) {
	if guid == "" {
		return nil, errNoIDs
	}
	if !strings.HasPrefix(guid, "plex://") {
		m := &metadata{GUID: guid}
		if t, v, i := m.ids(); t == 0 && v == 0 && i == "" {
			return nil, errNoIDs
		}
		return m, nil
	}
	m, err := c.provider(ctx, guid)
	if err == nil {
		if t, v, i := m.ids(); t != 0 || v != 0 || i != "" {
			return m, nil
		}
	}
	local := localGUIDs(ctx, db, guid)
	if local == nil {
		if err == nil {
			err = errNoIDs
		}
		return nil, err
	}
	if m != nil { // keep the provider's title, year and duration
		local.Duration, local.Title, local.Year = m.Duration, firstNonEmpty(m.Title, local.Title), max(m.Year, local.Year)
	}
	return local, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (c *Client) provider(ctx context.Context, guid string) (*metadata, error) {
	rk := guid[strings.LastIndex(guid, "/")+1:]
	c.provMu.Lock()
	if c.prov == nil {
		c.prov = map[string]provided{}
	}
	if p, ok := c.prov[rk]; ok && time.Since(p.when) < 7*24*time.Hour {
		c.provMu.Unlock()
		if p.err != nil {
			return nil, p.err
		}
		md := p.md
		return &md, nil
	}
	c.provMu.Unlock()

	base := c.ProviderURL
	if base == "" {
		base = providerURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/library/metadata/"+url.PathEscape(rk), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.Token)
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	var md metadata
	resp, err := hc.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("plex provider: HTTP %d", resp.StatusCode)
		} else {
			var w struct {
				MediaContainer struct{ Metadata []metadata }
			}
			if err = json.NewDecoder(resp.Body).Decode(&w); err == nil && len(w.MediaContainer.Metadata) > 0 {
				md = w.MediaContainer.Metadata[0]
			}
		}
	}
	if ctx.Err() == nil { // a cancelled lookup is not an answer
		c.provMu.Lock()
		c.prov[rk] = provided{md: md, err: err, when: time.Now()}
		c.provMu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	return &md, nil
}

// localGUIDs reads the external IDs Plex stored for a GUID in its library
// (tags of type 314); nil when the item is no longer there.
func localGUIDs(ctx context.Context, db *sql.DB, guid string) *metadata {
	if db == nil {
		return nil
	}
	var id int64
	var m metadata
	var year sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT id, COALESCE(title,''), year FROM metadata_items WHERE guid = ? LIMIT 1",
		guid).Scan(&id, &m.Title, &year); err != nil {
		return nil
	}
	m.Year = int(year.Int64)
	rows, err := db.QueryContext(ctx, `SELECT t.tag FROM taggings tg JOIN tags t ON t.id = tg.tag_id
		WHERE tg.metadata_item_id = ? AND t.tag_type = 314`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		if rows.Scan(&tag) == nil {
			m.Guid = append(m.Guid, struct {
				ID string `json:"id"`
			}{tag})
		}
	}
	if t, v, i := m.ids(); t == 0 && v == 0 && i == "" {
		return nil
	}
	return &m
}
