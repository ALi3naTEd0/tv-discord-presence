package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServesTheAppAndFallsBackToIndex(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/history", "/favicon.svg"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		if p != "/favicon.svg" && !strings.Contains(rec.Body.String(), `<div id="app">`) {
			t.Errorf("%s: not the app", p)
		}
	}
}
