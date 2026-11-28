package deliver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTokenRenewsExpired(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"access_token":"old","created_at":%d,"expires_in":604800}`, old)), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := fmt.Sprintf(`{"access_token":"new","created_at":%d,"expires_in":604800}`, time.Now().Unix())
	renew := []string{"sh", "-c", "printf '%s' '" + fresh + "' > " + path}

	tok, err := tokenFrom(path, renew)(context.Background())
	if err != nil || tok != "new" {
		t.Fatalf("got %q, %v; want the renewed token", tok, err)
	}

	// Without a renew command an expired token stays an error.
	os.WriteFile(path, []byte(fmt.Sprintf(`{"access_token":"old","created_at":%d,"expires_in":604800}`, old)), 0o600)
	if _, err := tokenFrom(path, nil)(context.Background()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("want an expired error, got %v", err)
	}
}
