package ul

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestListenNetworkKeepsAnIPv4LiteralOnIPv4(t *testing.T) {
	for address, want := range map[string]string{
		"0.0.0.0:104":          "tcp4",
		"127.0.0.1:0":          "tcp4",
		"192.168.1.20:11112":   "tcp4",
		"[::1]:104":            "tcp",
		"[::]:104":             "tcp",
		"[::ffff:127.0.0.1]:1": "tcp",
		":104":                 "tcp",
		"localhost:104":        "tcp",
		"not an address":       "tcp",
	} {
		if got := listenNetwork(address); got != want {
			t.Errorf("listenNetwork(%q) = %q, want %q", address, got, want)
		}
	}
}

// A listener on 0.0.0.0 accepts IPv4 connections and is not reachable over
// the IPv6 loopback.
func TestListenOnUnspecifiedIPv4IsNotReachableOverIPv6(t *testing.T) {
	// Listening on every interface makes Windows ask for a firewall
	// exception for each freshly built test binary.
	if os.Getenv("DICOMGO_TEST_EXTERNAL_BIND") != "1" {
		t.Skip("set DICOMGO_TEST_EXTERNAL_BIND=1 to listen on 0.0.0.0")
	}
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("this host has no IPv6 loopback: %v", err)
	}
	_ = probe.Close()

	listener, err := Listen(ListenOptions{Address: "0.0.0.0:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 2*time.Second)
	if err != nil {
		t.Fatalf("the listener is not reachable over IPv4: %v", err)
	}
	_ = conn.Close()
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort("::1", port), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("a listener on 0.0.0.0 accepted a connection over ::1")
	}
}
