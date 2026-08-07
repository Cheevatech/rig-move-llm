package proxy

import (
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/Cheevatech/rig-move-llm/internal/config"
)

// TestListenAddrDefaultsToLoopback pins the bind interface. Through 0.8.0 the
// server bound ":PORT" — every interface — while run, doctor and `serve --status`
// all dialled 127.0.0.1, so nothing in the product ever needed the wider bind.
// It mattered because the worker leg authenticates with WORKER_API_KEY out of
// config.env: a caller who can reach the port needs no credentials of their own
// to spend the endpoint behind it. Verified on a real install by driving the
// worker from another address on the LAN with no auth header at all.
func TestListenAddrDefaultsToLoopback(t *testing.T) {
	s := New(config.Config{Port: "4123", DataDir: t.TempDir()})
	if got, want := s.ListenAddr(), "127.0.0.1:4123"; got != want {
		t.Fatalf("ListenAddr() = %q, want %q", got, want)
	}

	// A Server built without New (a test, a future caller) must not be off-box
	// either: the zero value has to mean loopback, not "every interface".
	zero := &Server{cfg: config.Config{Port: "4123"}}
	if got, want := zero.ListenAddr(), "127.0.0.1:4123"; got != want {
		t.Fatalf("zero-value Server binds %q, want %q", got, want)
	}
}

func TestListenAddrHonoursAnExplicitBind(t *testing.T) {
	s := New(config.Config{Port: "4123", DataDir: t.TempDir()})
	s.Bind = "0.0.0.0"
	if got, want := s.ListenAddr(), "0.0.0.0:4123"; got != want {
		t.Fatalf("ListenAddr() = %q, want %q — exposing the proxy has to stay possible, just deliberate", got, want)
	}
}

// TestServerIsNotReachableOffLoopback is the behavioural half: bind the way the
// daemon does and confirm a non-loopback address of this machine cannot reach
// it. This is the assertion that fails if the bind goes back to ":" + port.
func TestServerIsNotReachableOffLoopback(t *testing.T) {
	external := nonLoopbackAddr(t)

	s := New(config.Config{Port: "0", DataDir: t.TempDir()})
	ln, err := net.Listen("tcp", s.ListenAddr())
	if err != nil {
		t.Fatalf("listen on %s: %v", s.ListenAddr(), err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: s.Handler()}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	// Loopback reaches it — otherwise the negative below proves nothing.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
	if err != nil {
		t.Fatalf("loopback cannot reach the proxy either, so this test proves nothing: %v", err)
	}
	conn.Close()

	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(external, port), 2*time.Second); err == nil {
		conn.Close()
		t.Fatalf("the proxy answered on %s — anyone who can route to this host can spend the worker endpoint", external)
	}
}

// nonLoopbackAddr returns an address of this machine that is not loopback, or
// skips: on a host with no other interface there is nothing to prove.
func nonLoopbackAddr(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface list: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	t.Skip("this host has no non-loopback IPv4 address to test against")
	return ""
}
