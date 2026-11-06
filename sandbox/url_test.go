package sandbox_test

// Tests for the sandbox network policy: scheme allowlist and the refusal of
// loopback/private/link-local/cloud-metadata destinations.

import (
	"net/url"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

func parseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return parsed
}

func TestCheckURLNetworkDisabledByDefault(t *testing.T) {
	policy := sandbox.New(sandbox.WithRoots(testRoot(t)))
	if err := policy.CheckURL(parseURL(t, "https://example.com/api")); err == nil {
		t.Fatal("network was allowed by default")
	} else {
		requireDenied(t, err, "default CheckURL")
	}
	if err := sandbox.New().CheckURL(parseURL(t, "https://example.com")); err == nil {
		t.Fatal("empty policy allowed network")
	}
}

func TestCheckURLSchemes(t *testing.T) {
	policy := sandbox.New(sandbox.WithNetwork(true))
	for _, raw := range []string{
		"http://example.com",
		"https://example.com/api?q=1",
	} {
		if err := policy.CheckURL(parseURL(t, raw)); err != nil {
			t.Fatalf("CheckURL(%q) denied: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"ftp://example.com/file",
		"file:///etc/passwd",
		"gopher://example.com",
		"javascript:alert(1)",
	} {
		if err := policy.CheckURL(parseURL(t, raw)); err == nil {
			t.Fatalf("CheckURL(%q) was allowed", raw)
		} else {
			requireDenied(t, err, "CheckURL("+raw+")")
		}
	}
	if err := policy.CheckURL(nil); err == nil {
		t.Fatal("nil URL was allowed")
	}
	if err := policy.CheckURL(&url.URL{Scheme: "https"}); err == nil {
		t.Fatal("URL without host was allowed")
	}
}

func TestCheckURLBlocksLocalAndMetadataHosts(t *testing.T) {
	policy := sandbox.New(sandbox.WithNetwork(true))
	for _, raw := range []string{
		"http://localhost/admin",
		"http://LOCALHOST:8080/",
		"http://foo.localhost/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://metadata/",
		"http://instance-data/latest/meta-data/",
	} {
		if err := policy.CheckURL(parseURL(t, raw)); err == nil {
			t.Fatalf("CheckURL(%q) was allowed", raw)
		} else {
			requireDenied(t, err, "CheckURL("+raw+")")
		}
	}
}

func TestCheckURLBlocksReservedAddresses(t *testing.T) {
	policy := sandbox.New(sandbox.WithNetwork(true))
	blocked := []string{
		"http://127.0.0.1/",
		"http://127.1.2.3/",
		"http://[::1]/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://172.31.255.255/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/",
		"http://[fd00::1]/",
		"http://[fd00:ec2::254]/",
		"http://0.0.0.0/",
		"http://0.1.2.3/",
		"http://100.100.100.200/",
		"http://198.18.0.1/",
		"http://240.0.0.1/",
		"http://224.0.0.1/",
		"http://192.0.2.1/",
		"http://198.51.100.1/",
		"http://203.0.113.1/",
	}
	for _, raw := range blocked {
		if err := policy.CheckURL(parseURL(t, raw)); err == nil {
			t.Errorf("CheckURL(%q) was allowed", raw)
		} else if !strings.Contains(err.Error(), "not allowed") {
			t.Errorf("CheckURL(%q) error = %v, want a not-allowed message", raw, err)
		}
	}
	allowed := []string{
		"http://8.8.8.8/",
		"https://1.1.1.1/",
		"https://[2001:4860:4860::8888]/",
		"https://example.com/",
	}
	for _, raw := range allowed {
		if err := policy.CheckURL(parseURL(t, raw)); err != nil {
			t.Errorf("CheckURL(%q) denied: %v", raw, err)
		}
	}
}

func TestCheckURLDedupesBlockedErrorText(t *testing.T) {
	policy := sandbox.New(sandbox.WithNetwork(true))
	err := policy.CheckURL(parseURL(t, "http://169.254.169.254/"))
	if err == nil {
		t.Fatal("metadata endpoint was allowed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "metadata") &&
		!strings.Contains(strings.ToLower(err.Error()), "link-local") {
		t.Fatalf("error = %v, want metadata/link-local reason", err)
	}
}
