package netflix

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func chromeUS(t time.Time) int64 { return (t.Unix()+11644473600)*1e6 + int64(t.Nanosecond()/1e3) }

func chromeStore(t *testing.T, dir string) *sql.DB {
	t.Helper()
	p := filepath.Join(dir, "Default", "Network")
	os.MkdirAll(p, 0o700)
	db, err := sql.Open("sqlite", filepath.Join(p, "Cookies"))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec("CREATE TABLE cookies (host_key TEXT, name TEXT, encrypted_value BLOB, creation_utc INTEGER, last_access_utc INTEGER)")
	t.Cleanup(func() { db.Close() })
	return db
}

func add(db *sql.DB, name string, created, accessed time.Time) {
	db.Exec("INSERT INTO cookies VALUES ('.netflix.com', ?, x'00', ?, ?)", name, chromeUS(created), chromeUS(accessed))
}

func TestCookieTimes(t *testing.T) {
	dir := t.TempDir()
	db := chromeStore(t, dir)
	now := time.Now().Truncate(time.Microsecond)
	add(db, "NetflixId", now, now.Add(time.Minute))
	db.Exec("INSERT INTO cookies VALUES ('.example.com', 'other', x'00', 1, 1)")
	c, err := cookieTimes(dir)
	if err != nil || len(c) != 1 || !c["NetflixId"].created.Equal(now) || !c["NetflixId"].accessed.Equal(now.Add(time.Minute)) {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestSignedInWaitsForANewSignInAndProfile(t *testing.T) {
	dir := t.TempDir()
	db := chromeStore(t, dir)
	old := time.Now().Add(-time.Hour)
	add(db, "NetflixId", old, old) // left from an earlier connection
	add(db, "profilesNewSession", old, old)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ch := signedIn(ctx, dir, start, make(chan struct{}))
	select {
	case <-ch:
		t.Fatal("fired on cookies from before the window opened")
	case <-time.After(2 * time.Second):
	}
	db.Exec("DELETE FROM cookies")
	now := time.Now()
	add(db, "NetflixId", now, now)
	add(db, "profilesNewSession", now.Add(time.Second), now.Add(2*time.Second))
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("did not notice the sign-in")
	}
}

func TestSignedInStopsWhenWindowCloses(t *testing.T) {
	stop := make(chan struct{})
	ch := signedIn(context.Background(), t.TempDir(), time.Now(), stop)
	close(stop)
	select {
	case <-ch:
		t.Fatal("fired without a sign-in")
	case <-time.After(2 * time.Second):
	}
}
