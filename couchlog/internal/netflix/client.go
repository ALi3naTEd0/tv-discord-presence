package netflix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client reads one Netflix profile's activity with a session.
type Client struct {
	// Cookies returns the session's netflix.com cookies; nil = BrowserCookies.
	Cookies func() (map[string]string, string, error)
	// SessionPath, when the cookies came from Couchlog's saved session,
	// is updated with the cookies Netflix renews, so the session lasts.
	SessionPath string
	BaseURL     string // "" = https://www.netflix.com (tests override)
	HTTP        *http.Client

	mu      sync.Mutex
	profile *Profile
	profAt  time.Time
	meta    map[int64]*Video
}

// Profile is what the viewing-activity page says about the signed-in profile.
type Profile struct {
	AuthURL string
	GUID    string
	Name    string
	Build   string
}

// Viewed is one entry of the viewing activity: Netflix keeps only the
// latest viewing of each title, with when it started.
type Viewed struct {
	MovieID      int64  `json:"movieID"` // the episode, or the movie
	Series       int64  `json:"series"`  // 0 for movies
	Title        string `json:"title"`
	SeriesTitle  string `json:"seriesTitle"`
	EpisodeTitle string `json:"episodeTitle"`
	DateMS       int64  `json:"date"`
}

// Started is when Netflix says this title started playing.
func (v Viewed) Started() time.Time { return time.UnixMilli(v.DateMS) }

// Video is a title's metadata: a show with its seasons and episodes, or a movie.
type Video struct {
	ID                   int64  `json:"id"`
	Type                 string `json:"type"` // show | movie
	Title                string `json:"title"`
	Year                 int    `json:"year"`
	HiddenEpisodeNumbers bool   `json:"hiddenEpisodeNumbers"`
	Seasons              []struct {
		Seq      int `json:"seq"`
		Episodes []struct {
			ID    int64  `json:"id"`
			Seq   int    `json:"seq"`
			Title string `json:"title"`
		} `json:"episodes"`
	} `json:"seasons"`
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://www.netflix.com"
}

func (c *Client) do(ctx context.Context, method, u, body string, headers map[string]string) ([]byte, error) {
	get := c.Cookies
	if get == nil {
		get = BrowserCookies
	}
	cookies, src, err := get()
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cookieHeader(cookies))
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0")
	req.Header.Set("Accept", "*/*")
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 25 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("netflix: %w", err)
	}
	defer resp.Body.Close()
	if c.SessionPath != "" && strings.HasPrefix(src, "couchlog") {
		c.keepRenewed(resp.Cookies())
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("netflix: HTTP %d on %s", resp.StatusCode, strings.SplitN(u, "?", 2)[0])
	}
	return b, nil
}

var (
	reAuthURL = regexp.MustCompile(`"authURL":"(.*?)"`)
	// Netflix serves two variants of the page: the profile is under
	// userInfo or auiUserContext.
	reGUID  = regexp.MustCompile(`"(?:userInfo|auiUserContext)":\{"data":\{[^}]*?"userGuid":"(.*?)"`)
	reName  = regexp.MustCompile(`"(?:userInfo|auiUserContext)":\{"data":\{"name":"(.*?)"`)
	reBuild = regexp.MustCompile(`"BUILD_IDENTIFIER":"(.*?)"`)
	reJSHex = regexp.MustCompile(`\\x([0-9A-Fa-f]{2})|\\u([0-9A-Fa-f]{4})`)
)

// jsUnescape decodes the escapes of a JavaScript string literal in the page.
func jsUnescape(s string) string {
	s = reJSHex.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
	return strings.ReplaceAll(s, `\/`, "/")
}

// ErrExpired means Netflix did not recognize the session.
var ErrExpired = fmt.Errorf("netflix: session expired or no profile chosen (open netflix.com in the browser)")

// Profile reads the signed-in profile (cached for an hour).
func (c *Client) Profile(ctx context.Context) (*Profile, error) {
	c.mu.Lock()
	if c.profile != nil && time.Since(c.profAt) < time.Hour {
		p := c.profile
		c.mu.Unlock()
		return p, nil
	}
	c.mu.Unlock()
	b, err := c.do(ctx, http.MethodGet, c.base()+"/settings/viewed/", "", nil)
	if err != nil {
		return nil, err
	}
	grab := func(rx *regexp.Regexp) string {
		if m := rx.FindSubmatch(b); m != nil {
			return jsUnescape(string(m[1]))
		}
		return ""
	}
	p := &Profile{AuthURL: grab(reAuthURL), GUID: grab(reGUID), Name: grab(reName), Build: grab(reBuild)}
	if p.AuthURL == "" || p.GUID == "" {
		return nil, ErrExpired
	}
	c.mu.Lock()
	c.profile, c.profAt = p, time.Now()
	c.mu.Unlock()
	return p, nil
}

// Activity is a page of the profile's viewing activity, newest first.
func (c *Client) Activity(ctx context.Context, page, size int) ([]Viewed, error) {
	p, err := c.Profile(ctx)
	if err != nil {
		return nil, err
	}
	path := url.QueryEscape(fmt.Sprintf(`["aui","viewingActivity",%d,%d]`, page, size))
	u := c.base() + "/api/aui/pathEvaluator/web/%5E2.0.0?method=call&callPath=" + path + "&falcor_server=0.1.0"
	param, _ := json.Marshal(map[string]string{"guid": p.GUID})
	b, err := c.do(ctx, http.MethodPost, u, "param="+url.QueryEscape(string(param)), map[string]string{
		"x-netflix.request.routing": `{"path":"/nq/aui/endpoint/%5E1.0.0-web/pathEvaluator","control_tag":"auinqweb"}`,
	})
	if err != nil {
		return nil, err
	}
	var r struct {
		JSONGraph struct {
			AUI struct {
				ViewingActivity struct {
					Value struct {
						ViewedItems []Viewed `json:"viewedItems"`
					} `json:"value"`
				} `json:"viewingActivity"`
			} `json:"aui"`
		} `json:"jsonGraph"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("netflix: activity: %w", err)
	}
	return r.JSONGraph.AUI.ViewingActivity.Value.ViewedItems, nil
}

// Metadata reads a show (all seasons and episodes) or a movie, with English
// titles, trying the routes Netflix has used (it moves them).
func (c *Client) Metadata(ctx context.Context, id int64) (*Video, error) {
	c.mu.Lock()
	if v, ok := c.meta[id]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	p, err := c.Profile(ctx)
	if err != nil {
		return nil, err
	}
	bases := []string{c.base() + "/nq/website/memberapi/release"}
	if p.Build != "" {
		bases = append(bases, c.base()+"/nq/website/memberapi/"+p.Build, c.base()+"/api/shakti/"+p.Build)
	}
	bases = append(bases, c.base()+"/api/shakti/mre")
	var v *Video
	for _, b := range bases {
		for _, extra := range []string{"", "&authURL=" + url.QueryEscape(p.AuthURL)} {
			body, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/metadata?languages=en-US&movieid=%d%s", b, id, extra), "", nil)
			if err != nil {
				continue
			}
			var r struct {
				Video *Video `json:"video"`
			}
			if json.Unmarshal(body, &r) == nil && r.Video != nil {
				v = r.Video
				break
			}
		}
		if v != nil {
			break
		}
	}
	if v == nil {
		return nil, fmt.Errorf("netflix: no metadata for %d", id)
	}
	c.mu.Lock()
	if c.meta == nil {
		c.meta = map[int64]*Video{}
	}
	c.meta[id] = v
	c.mu.Unlock()
	return v, nil
}

// keepRenewed stores the session cookies Netflix sends back renewed.
func (c *Client) keepRenewed(renewed []*http.Cookie) {
	var changed []*http.Cookie
	for _, k := range renewed {
		if (k.Name == "NetflixId" || k.Name == "SecureNetflixId") && k.Value != "" && k.MaxAge >= 0 {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := LoadSaved(c.SessionPath)
	if err != nil {
		return
	}
	dirty := false
	for _, k := range changed {
		if s.Cookies[k.Name] != k.Value {
			s.Cookies[k.Name], dirty = k.Value, true
		}
	}
	if dirty {
		s.SavedAt = time.Now()
		s.Save(c.SessionPath)
	}
}
