package hook

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// #22, as measured: a keyed worker endpoint answered 401 to the anonymous probe,
// rig filed it next to connection-refused, and the hybrid turned itself off for
// the whole session while the worker leg was working fine.
func TestProbeAuthenticatesWhenAKeyIsConfigured(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		if seen != "Bearer sk-worker" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if !Probe(srv.URL, time.Second, "sk-worker") {
		t.Error("a keyed endpoint that accepts the key must read as healthy")
	}
	if seen != "Bearer sk-worker" {
		t.Errorf("probe sent Authorization %q, want the configured bearer token", seen)
	}
}

// "Is there a worker to delegate to" is not "is this request authorized". An
// endpoint that answers at all is alive; only a failing server (or nothing
// listening) is dead.
func TestProbeClassifiesStatuses(t *testing.T) {
	cases := []struct {
		status int
		alive  bool
		why    string
	}{
		{http.StatusOK, true, "2xx is the normal healthy answer"},
		{http.StatusUnauthorized, true, "401 is the endpoint saying it is there"},
		{http.StatusForbidden, true, "403 is a permission problem, not a dead worker"},
		{http.StatusNotFound, true, "404 means the probe path is wrong, not the host gone"},
		{http.StatusInternalServerError, false, "5xx is a failing server"},
		{http.StatusBadGateway, false, "502 is how a down upstream looks through a loader"},
		{http.StatusServiceUnavailable, false, "503 is a loader with nothing behind it"},
	}
	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			if got := Probe(srv.URL, time.Second, ""); got != c.alive {
				t.Errorf("Probe on %d = %v, want %v — %s", c.status, got, c.alive, c.why)
			}
		})
	}
}

// A probe with no key configured must stay anonymous: sending an empty bearer
// token can be worse than sending nothing on providers that parse the header.
func TestProbeStaysAnonymousWithoutAKey(t *testing.T) {
	var hadHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadHeader = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	Probe(srv.URL, time.Second, "   ")
	if hadHeader {
		t.Error("probe sent an Authorization header without a configured key")
	}
}
