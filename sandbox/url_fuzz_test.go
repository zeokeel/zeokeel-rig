package sandbox_test

// Fuzz targets for the outbound-URL policy. CheckURL is entirely syntactic: it
// never resolves a hostname (that would block on DNS and is vulnerable to
// rebinding), so this target is deterministic and needs no network. Arbitrary
// URL text must never panic, and the policy must never allow a destination the
// contract says is forbidden.

import (
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// fuzzMaxInput bounds one fuzz input; URLs are short, so a generous cap keeps
// every execution cheap.
const fuzzMaxInput = 1 << 20

func FuzzCheckURL(f *testing.F) {
	seeds := []string{
		"https://example.com/path?q=1#frag",
		"http://example.com",
		"HTTP://EXAMPLE.COM:8080/",
		"https://user:pass@example.com/",
		"http://localhost:8080/",
		"http://localhost./",
		"http://app.localhost/",
		"http://127.0.0.1/",
		"http://127.1.2.3/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://metadata/",
		"http://instance-data.ec2.internal/",
		"http://metadata.tencentyun.com/",
		"file:///etc/passwd",
		"ftp://example.com/",
		"javascript:alert(1)",
		"data:text/plain,hello",
		"gopher://example.com/",
		"https://",
		"http://",
		"://broken",
		"http://example.com:99999/",
		"http://[::ffff:127.0.0.1]/",
		"",
		"   ",
		"https://example.com/\x00",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	networkPolicy := sandbox.New(sandbox.WithNetwork(true))
	// A policy that never enabled network access must deny every URL, including
	// perfectly public ones (fail closed).
	closedPolicy := sandbox.New()

	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > fuzzMaxInput {
			raw = raw[:fuzzMaxInput]
		}
		parsed, err := url.Parse(raw)
		if err != nil {
			return // unparseable input never reaches the policy
		}

		if err := closedPolicy.CheckURL(parsed); err == nil {
			t.Fatalf("network-disabled policy allowed %.80q", raw)
		}
		if err := networkPolicy.CheckURL(parsed); err != nil {
			return // denying is always a safe outcome
		}

		// Allowed: the scheme and host rules must hold.
		scheme := strings.ToLower(strings.TrimSpace(parsed.Scheme))
		if scheme != "http" && scheme != "https" {
			t.Fatalf("allowed non-HTTP(S) scheme %q for %.80q", scheme, raw)
		}
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(parsed.Hostname()), "."))
		if host == "" {
			t.Fatalf("allowed a URL without a host: %.80q", raw)
		}

		// Loopback, metadata hostnames and private/link-local IP literals are
		// never allowed, even with network access enabled.
		switch {
		case host == "localhost", strings.HasSuffix(host, ".localhost"):
			t.Fatalf("allowed loopback host %q for %.80q", host, raw)
		case host == "metadata", host == "metadata.google.internal", host == "metadata.goog",
			host == "instance-data", host == "instance-data.ec2.internal",
			host == "metadata.azure.internal", host == "metadata.tencentyun.com":
			t.Fatalf("allowed cloud metadata host %q for %.80q", host, raw)
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
				ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
				ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
				t.Fatalf("allowed non-public address %s for %.80q", ip, raw)
			}
		}
	})
}
