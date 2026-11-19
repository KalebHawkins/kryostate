package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoopbackOnlyRefusesOtherNames: on a loopback listener, a request whose
// Host is another name (a page using DNS rebinding) is refused, and loopback
// names with or without a port are served.
func TestLoopbackOnlyRefusesOtherNames(t *testing.T) {
	h := loopbackHosts(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for host, want := range map[string]int{
		"localhost:8099":            http.StatusOK,
		"127.0.0.1:8099":            http.StatusOK,
		"[::1]:8099":                http.StatusOK,
		"LOCALHOST.":                http.StatusOK,
		"attacker.example.com":      http.StatusMisdirectedRequest,
		"attacker.example.com:8099": http.StatusMisdirectedRequest,
		"192.0.2.10:8099":           http.StatusMisdirectedRequest,
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.Code, want)
		}
	}
}
