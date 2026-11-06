package sandbox

// Internal tests for ResolveHost: the DNS lookup is injected so the validation
// rules are tested without touching a real resolver.

import (
	"context"
	"errors"
	"net"
	"testing"
)

func policyWithLookup(ips []net.IP, lookupErr error) *Policy {
	policy := New(WithNetwork(true))
	policy.lookupIP = func(context.Context, string) ([]net.IP, error) {
		return ips, lookupErr
	}
	return policy
}

func TestResolveHostBlocksIPLiterals(t *testing.T) {
	policy := New(WithNetwork(true))
	for _, host := range []string{"127.0.0.1", "::1", "10.0.0.5", "169.254.169.254", "192.168.1.2", "0.0.0.0"} {
		if _, err := policy.ResolveHost(context.Background(), host); !errors.Is(err, ErrDenied) {
			t.Errorf("ResolveHost(%q) = %v, want ErrDenied", host, err)
		}
	}
	ips, err := policy.ResolveHost(context.Background(), "93.184.216.34")
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP("93.184.216.34")) {
		t.Fatalf("ResolveHost(public literal) = %v, %v", ips, err)
	}
}

func TestResolveHostBlocksDNSAliasToLoopback(t *testing.T) {
	policy := policyWithLookup([]net.IP{net.ParseIP("127.0.0.1")}, nil)
	if _, err := policy.ResolveHost(context.Background(), "127.0.0.1.nip.io"); !errors.Is(err, ErrDenied) {
		t.Fatalf("alias to loopback = %v, want ErrDenied", err)
	}
}

func TestResolveHostDeniesMixedPublicAndPrivateAnswers(t *testing.T) {
	policy := policyWithLookup([]net.IP{
		net.ParseIP("93.184.216.34"),
		net.ParseIP("169.254.169.254"), // cloud metadata
	}, nil)
	if _, err := policy.ResolveHost(context.Background(), "mixed.example"); !errors.Is(err, ErrDenied) {
		t.Fatalf("mixed public/private answers = %v, want ErrDenied", err)
	}
}

func TestResolveHostAllowsPublicAndPrefersIPv4(t *testing.T) {
	policy := policyWithLookup([]net.IP{
		net.ParseIP("2606:4700:4700::1111"),
		net.ParseIP("93.184.216.34"),
	}, nil)
	ips, err := policy.ResolveHost(context.Background(), "public.example")
	if err != nil {
		t.Fatalf("ResolveHost = %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("ResolveHost returned %d addresses, want 2", len(ips))
	}
	if ips[0].To4() == nil {
		t.Fatalf("first address = %v, want the IPv4 address first", ips[0])
	}
}

func TestResolveHostRefusesMetadataNamesWithoutLookup(t *testing.T) {
	policy := New(WithNetwork(true))
	policy.lookupIP = func(context.Context, string) ([]net.IP, error) {
		t.Fatal("lookupIP must not be called for a known metadata host")
		return nil, nil
	}
	if _, err := policy.ResolveHost(context.Background(), "metadata.google.internal"); !errors.Is(err, ErrDenied) {
		t.Fatalf("metadata host = %v, want ErrDenied", err)
	}
}

func TestResolveHostDeniesWhenNetworkDisabled(t *testing.T) {
	policy := New()
	if _, err := policy.ResolveHost(context.Background(), "example.com"); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled network = %v, want ErrDenied", err)
	}
	var nilPolicy *Policy
	if _, err := nilPolicy.ResolveHost(context.Background(), "example.com"); !errors.Is(err, ErrDenied) {
		t.Fatalf("nil policy = %v, want ErrDenied", err)
	}
}

func TestResolveHostLookupFailureIsDenied(t *testing.T) {
	policy := policyWithLookup(nil, errors.New("no such host"))
	if _, err := policy.ResolveHost(context.Background(), "missing.example"); !errors.Is(err, ErrDenied) {
		t.Fatalf("lookup failure = %v, want ErrDenied", err)
	}
	policy = policyWithLookup(nil, nil)
	if _, err := policy.ResolveHost(context.Background(), "empty.example"); !errors.Is(err, ErrDenied) {
		t.Fatalf("empty answer = %v, want ErrDenied", err)
	}
}
