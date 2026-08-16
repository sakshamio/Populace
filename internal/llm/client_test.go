package llm

import (
	"context"
	"testing"
)

// The CGNAT mask is the part worth testing. 100.64.0.0/10 is Tailscale's range,
// but 100.x is NOT all tailnet -- 100.1.2.3 is ordinary public space, and a
// check that treated the whole /8 as private would reject a legitimate gateway
// with a message telling the operator to fix something that is not broken.
//
// The loopback and RFC1918 cases are here as regressions: an earlier version
// flagged them, which would have warned about every same-host deployment --
// including the local Docker run used to verify the image before deploying.
func TestLooksLikeTailnet(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://gateway.example.com", false},
		{"http://100.101.102.103:8091", true},   // tailnet, inside the /10
		{"http://100.127.255.254:8091", true},   // top of the /10
		{"http://100.64.0.0:8091", true},        // first address of the /10
		{"http://100.63.255.255:8091", false},   // one below: public
		{"http://100.128.0.1:8091", false},      // one above: public
		{"http://100.1.2.3:8091", false},        // public 100/8
		{"http://dgx-spark-k3:8091", true},      // bare name: MagicDNS only
		{"http://127.0.0.1:8091", false},        // same host: always routable
		{"http://localhost:8091", false},        // same host, by name
		{"http://192.168.1.10:8091", false},     // same LAN: legitimate
		{"http://10.0.0.5:8091", false},
		{"http://[::1]:8091", false},
		{"https://populace-production.up.railway.app", false},
	} {
		if got := looksLikeTailnet(tc.url); got != tc.want {
			t.Errorf("looksLikeTailnet(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestCheckRejectsUnsetURL(t *testing.T) {
	if err := New("", "tok").Check(context.Background()); err == nil {
		t.Fatal("empty BaseURL should be reported, not silently accepted")
	}
}

// A private address with no proxy must be named specifically rather than left
// to time out -- that ten-minute hang is the failure this function exists for.
func TestCheckNamesTailnetAddressWithoutProxy(t *testing.T) {
	t.Setenv("ALL_PROXY", "")
	t.Setenv("all_proxy", "")
	err := New("http://100.101.102.103:8091", "tok").Check(context.Background())
	if err == nil {
		t.Fatal("tailnet address without ALL_PROXY should fail fast")
	}
	// It must not have tried the network: a real dial to a tailnet address
	// from a machine with no route is exactly the hang being prevented.
	if got := err.Error(); !contains(got, "tailnet") {
		t.Errorf("error should name the cause, got: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
