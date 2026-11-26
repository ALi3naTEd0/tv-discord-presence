package presence

import (
	"fmt"
	"log/slog"
	"time"
)

// DefaultClientID is Couchlog's own Discord application, put into release
// builds at compile time (-ldflags "-X .../presence.DefaultClientID=...").
var DefaultClientID string

// Runner keeps Discord showing what plays: it sends a status only when it
// changes (or after a seek), and reconnects when Discord comes back.
type Runner struct {
	ClientID string
	// DryRun logs the status instead of sending it (the parallel run,
	// while tvpresence owns your Discord status).
	DryRun bool
	Log    *slog.Logger

	conn    *Conn
	retryAt time.Time
	lastKey string
}

func key(a *Activity) string {
	if a == nil {
		return ""
	}
	k := a.Line()
	if a.Assets != nil {
		k += "|" + a.Assets.LargeImage
	}
	if a.Timestamps != nil {
		k += fmt.Sprintf("|%d", a.Timestamps.Start/20) // a seek moves the start
	}
	return k
}

// Show updates the status for what plays now (nil clears it).
func (r *Runner) Show(n *Now, now time.Time) {
	a := Build(n, now)
	k := key(a)
	if k == r.lastKey {
		return
	}
	if r.DryRun {
		r.lastKey = k
		r.Log.Info("presence (dry run)", "status", a.Line())
		return
	}
	if r.conn == nil {
		if now.Before(r.retryAt) {
			return
		}
		c, err := Dial(r.ClientID)
		if err != nil {
			r.retryAt = now.Add(30 * time.Second)
			r.Log.Debug("Discord unavailable", "err", err)
			return
		}
		r.conn = c
		r.Log.Info("connected to Discord")
	}
	if err := r.conn.SetActivity(a); err != nil {
		r.Log.Warn("Discord status", "err", err)
		r.conn.Close()
		r.conn, r.lastKey = nil, ""
		return
	}
	r.lastKey = k
	r.Log.Info("presence", "status", a.Line())
}

// Close clears the status and disconnects.
func (r *Runner) Close() {
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}
