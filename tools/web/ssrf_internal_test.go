package web

import (
	"net"
	"testing"
)

// TestIsPublicIP covers the SSRF guard fallback used for policies that do not
// implement HostResolver: the fetcher must never dial these address classes.
func TestIsPublicIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"0.0.0.0", "100.64.0.1", "198.18.0.1", "240.0.0.1", "::1", "fe80::1", "fc00::1",
	}
	for _, raw := range blocked {
		if isPublicIP(net.ParseIP(raw)) {
			t.Errorf("isPublicIP(%s) = true, want false", raw)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888"}
	for _, raw := range allowed {
		if !isPublicIP(net.ParseIP(raw)) {
			t.Errorf("isPublicIP(%s) = false, want true", raw)
		}
	}
}
