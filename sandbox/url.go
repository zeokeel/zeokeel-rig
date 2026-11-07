package sandbox

import (
	"context"
	"net"
	"net/url"
	"slices"
	"strings"
)

// metadataHosts 是众所周知的云元数据服务名。它们的 IP 已被地址检查拒绝；列出
// 这些名字是为了让主机名无需解析就能被拒绝。
var metadataHosts = map[string]struct{}{
	"metadata":                   {},
	"metadata.google.internal":   {},
	"metadata.goog":              {},
	"instance-data":              {},
	"instance-data.ec2.internal": {},
	"metadata.azure.internal":    {},
	"metadata.tencentyun.com":    {},
}

// CheckURL 按网络策略校验一个对外 URL。
//
// 规则：
//   - 必须启用网络访问（WithNetwork(true)）；
//   - 只允许 http 与 https 协议；
//   - 必须存在主机名；
//   - "localhost"、*.localhost、云元数据主机名，以及任何处于 loopback、私有、
//     链路本地、组播、未指定、共享地址（100.64/10）、文档、基准测试或保留地址段
//     的 IP 字面量都会被拒绝——即使网络访问已启用。
//
// 这里不解析主机名：策略检查不能阻塞在 DNS 上，而且答案可能在检查与使用之间
// 变化（DNS 重绑定）。建立连接的网络客户端应调用 ResolveHost，并拨号到返回的
// （已校验、已固定的）地址；tools/web 在把策略作为其 Policy 传入时会自动这样做。
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
	// 公共主机名：允许，但这里不解析它（见文档注释）。
	return nil
}

// blockedIPReason 在 ip 不是公共单播地址时返回可读原因，允许时返回 ""。
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

// ResolveHost 解析 host，并按与 CheckURL 相同的规则校验它解析出的每个地址。它
// 返回允许的地址（IPv4 在前），使网络客户端可以拨号到已固定的 IP，从而关闭仅靠
// CheckURL 仍会留下的 DNS 重绑定缺口。云元数据名字无需查询就会被拒绝；nil 策略
// 或未启用网络的策略拒绝。只要有一个解析结果是被封禁地址，整个主机都会被拒绝。
func (p *Policy) ResolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if p == nil {
		return nil, deny("nil policy")
	}
	if !p.network {
		return nil, deny("network access is disabled")
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return nil, deny("host is empty")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, deny("loopback host %q is not allowed", host)
	}
	if _, blocked := metadataHosts[host]; blocked {
		return nil, deny("cloud metadata host %q is not allowed", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if reason := blockedIPReason(ip); reason != "" {
			return nil, deny("address %s is %s and is not allowed", ip, reason)
		}
		return []net.IP{ip}, nil
	}
	lookup := p.lookupIP
	if lookup == nil {
		lookup = defaultLookupIP
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, deny("cannot resolve host %q: %v", host, err)
	}
	if len(ips) == 0 {
		return nil, deny("host %q resolved to no addresses", host)
	}
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if reason := blockedIPReason(ip); reason != "" {
			return nil, deny("host %q resolves to %s, which is %s and is not allowed", host, ip, reason)
		}
		allowed = append(allowed, ip)
	}
	slices.SortStableFunc(allowed, func(a, b net.IP) int {
		a4, b4 := a.To4() != nil, b.To4() != nil
		switch {
		case a4 == b4:
			return 0
		case a4:
			return -1
		default:
			return 1
		}
	})
	return allowed, nil
}

func defaultLookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}
