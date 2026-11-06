package sandbox

import (
	"net"
	"net/url"
	"strings"
)

// metadataHosts are well-known cloud metadata service names. Their IPs are
// already refused by the address checks; the names are listed so a hostname
// never has to be resolved to be denied.
var metadataHosts = map[string]struct{}{
	"metadata":                   {},
	"metadata.google.internal":   {},
	"metadata.goog":              {},
	"instance-data":              {},
	"instance-data.ec2.internal": {},
	"metadata.azure.internal":    {},
	"metadata.tencentyun.com":    {},
}

// CheckURL validates one outgoing URL against the network policy.
//
// Rules:
//   - network access must be enabled (WithNetwork(true));
//   - only http and https schemes are allowed;
//   - the host must be present;
//   - "localhost", *.localhost, cloud metadata hostnames, and any IP literal in
//     loopback, private, link-local, multicast, unspecified, shared (100.64/10),
//     documentation, benchmarking or reserved ranges are refused, even when
//     network access is enabled.
//
// Hostnames are not resolved here: a policy check must not block on DNS, and the
// answer can change between check and use (DNS rebinding). A host that actually
// opens connections should resolve the name itself and reject non-public
// addresses, or pin the resulting IP.
func (p *Policy) CheckURL(u *url.URL) error {
	if p == nil {
		return deny("nil policy")
	}
	if u == nil {
		return deny("nil URL")
	}
	if !p.network {
		return deny("network access is disabled")
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	switch scheme {
	case "http", "https":
	default:
		return deny("scheme %q is not allowed (want http or https)", u.Scheme)
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if host == "" {
		return deny("URL has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return deny("loopback host %q is not allowed", host)
	}
	if _, blocked := metadataHosts[host]; blocked {
		return deny("cloud metadata host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if reason := blockedIPReason(ip); reason != "" {
			return deny("address %s is %s and is not allowed", ip, reason)
		}
		return nil
	}
	// Public hostname: allowed without resolving it (see doc comment).
	return nil
}

// blockedIPReason returns a human-readable reason when ip is not a public
// unicast address, or "" when it is allowed.
func blockedIPReason(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsLoopback():
		return "loopback"
	case ip.IsPrivate():
		return "private"
	case ip.IsLinkLocalUnicast():
		return "link-local"
	case ip.IsLinkLocalMulticast():
		return "link-local multicast"
	case ip.IsInterfaceLocalMulticast():
		return "interface-local multicast"
	case ip.IsMulticast():
		return "multicast"
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	switch {
	case ip4[0] == 0:
		return "in the 0.0.0.0/8 network"
	case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
		return "shared address space (100.64.0.0/10, includes some cloud metadata)"
	case ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 169 && ip4[3] == 254:
		return "the cloud metadata endpoint"
	case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0:
		return "IETF protocol assignments"
	case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2:
		return "documentation range (192.0.2.0/24)"
	case ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100:
		return "documentation range (198.51.100.0/24)"
	case ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113:
		return "documentation range (203.0.113.0/24)"
	case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19):
		return "benchmarking range (198.18.0.0/15)"
	case ip4[0] >= 240:
		return "reserved range (240.0.0.0/4)"
	}
	return ""
}
