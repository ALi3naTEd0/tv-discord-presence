package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Client is the HTTP side shared by the destinations: JSON in and out, at
// most one write per Interval (both services ask for that), and waiting out
// a 429's Retry-After a few times.
type Client struct {
	HTTP     *http.Client
	Interval time.Duration // between writes; 0 = 1.1 s
	Sleep    func(context.Context, time.Duration) error

	mu        sync.Mutex
	lastWrite time.Time
}

// Status is an HTTP answer that was not a success.
type Status struct {
	Code int
	Body string
}

func (e *Status) Error() string { return fmt.Sprintf("HTTP %d: %.200s", e.Code, e.Body) }

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// Do sends a request with an optional JSON body and decodes a JSON answer
// into out (when not nil). write marks requests that change your history.
func (c *Client) Do(ctx context.Context, method, url string, headers map[string]string, body, out any, write bool) error {
	if write {
		c.mu.Lock()
		iv := c.Interval
		if iv == 0 {
			iv = 1100 * time.Millisecond
		}
		wait := iv - time.Since(c.lastWrite)
		c.mu.Unlock()
		if wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
		}
		defer func() { c.mu.Lock(); c.lastWrite = time.Now(); c.mu.Unlock() }()
	}
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 40 * time.Second}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Couchlog")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			if wait <= 0 {
				wait = 2
			}
			if err := c.sleep(ctx, time.Duration(wait)*time.Second); err != nil {
				return err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return &Status{Code: resp.StatusCode, Body: string(b)}
		}
		if out != nil && len(b) > 0 {
			return json.Unmarshal(b, out)
		}
		return nil
	}
}
