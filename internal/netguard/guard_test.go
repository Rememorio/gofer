package netguard

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestURLGuardValidatesPublicRequests(t *testing.T) {
	t.Parallel()

	fixture := []netip.Addr{netip.MustParseAddr("93.184.216.34")}
	resolver := fakeResolver{addresses: map[string][]netip.Addr{"example.com": fixture}}
	guard, err := NewURLGuard(URLGuardConfig{Resolver: resolver})
	if err != nil {
		t.Fatalf("NewURLGuard(): %v", err)
	}
	validated, err := guard.ValidateNavigation(context.Background(), "https://example.com/path?q=1#fragment")
	if err != nil || validated != "https://example.com/path?q=1#fragment" {
		t.Fatalf("ValidateNavigation() = %q, %v", validated, err)
	}
	for _, rawURL := range []string{"data:text/plain,ok", "blob:https://example.com/id", "about:blank", "https://example.com"} {
		if err := guard.ValidateRequest(context.Background(), rawURL); err != nil {
			t.Fatalf("ValidateRequest(%q): %v", rawURL, err)
		}
	}
	if !reflect.DeepEqual(fixture, []netip.Addr{netip.MustParseAddr("93.184.216.34")}) {
		t.Fatal("resolver fixture was mutated")
	}
}

func TestURLGuardBlocksUnsafeAddresses(t *testing.T) {
	t.Parallel()

	for _, address := range []string{
		"0.0.0.1", "127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254",
		"100.64.0.1", "192.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "::", "::1", "fe80::1", "fc00::1", "ff02::1", "2001:db8::1",
		"64:ff9b::c0a8:1", "64:ff9b:1::1", "100::1", "2001::1", "2001:20::1", "2002:c0a8:1::", "3fff::1", "5f00::1",
	} {
		guard, err := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{
			"blocked.test": {netip.MustParseAddr(address)},
		}}})
		if err != nil {
			t.Fatalf("NewURLGuard(): %v", err)
		}
		if _, err := guard.ValidateNavigation(context.Background(), "http://blocked.test"); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("ValidateNavigation(%s) error = %v, want ErrUnsafeURL", address, err)
		}
	}
	guard, _ := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{
		"mixed.test": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")},
	}}})
	if _, err := guard.ValidateNavigation(context.Background(), "https://mixed.test"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("mixed-address error = %v, want ErrUnsafeURL", err)
	}
}

func TestURLGuardRejectsInvalidTargets(t *testing.T) {
	t.Parallel()

	guard, _ := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{err: errors.New("DNS unavailable")}})
	for _, rawURL := range []string{
		"", "relative", "://bad", "file:///etc/passwd", "ftp://example.com", "https://user@example.com",
		"http://localhost", "http://service.localhost", "http://missing.test",
	} {
		if _, err := guard.ValidateNavigation(context.Background(), rawURL); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("ValidateNavigation(%q) error = %v", rawURL, err)
		}
	}
	if err := guard.ValidateRequest(context.Background(), "file:///etc/passwd"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("ValidateRequest(file) error = %v", err)
	}
	var nilGuard *URLGuard
	if _, err := nilGuard.ValidateNavigation(context.Background(), "https://example.com"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil ValidateNavigation() error = %v", err)
	}
	if err := nilGuard.ValidateRequest(context.Background(), "https://example.com"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil ValidateRequest() error = %v", err)
	}
}

func TestURLGuardPrivateOptInAndResolutionFailures(t *testing.T) {
	t.Parallel()

	guard, err := NewURLGuard(URLGuardConfig{AllowPrivateAddresses: true})
	if err != nil {
		t.Fatalf("NewURLGuard(): %v", err)
	}
	if _, err := guard.ValidateNavigation(context.Background(), "http://127.0.0.1:9222"); err != nil {
		t.Fatalf("ValidateNavigation(private opt-in): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := guard.ValidateNavigation(ctx, "https://example.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateNavigation(cancelled) error = %v", err)
	}
	if _, err := NewURLGuard(URLGuardConfig{ResolveTimeout: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewURLGuard(negative) error = %v", err)
	}

	timed, _ := NewURLGuard(URLGuardConfig{
		ResolveTimeout: time.Millisecond,
		Resolver: fakeResolver{lookup: func(ctx context.Context) ([]netip.Addr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	})
	if _, err := timed.ValidateNavigation(context.Background(), "https://slow.test"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("ValidateNavigation(timeout) error = %v", err)
	}
	empty, _ := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{"empty.test": {}}}})
	if _, err := empty.ValidateNavigation(context.Background(), "https://empty.test"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("ValidateNavigation(empty) error = %v", err)
	}
}

func TestURLGuardDialContextUsesApprovedAddresses(t *testing.T) {
	t.Parallel()

	dialer := &fakeDialer{failures: 1}
	guard, err := NewURLGuard(URLGuardConfig{
		Resolver: fakeResolver{addresses: map[string][]netip.Addr{
			"service.test": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("1.1.1.1")},
		}},
		Dialer: dialer,
	})
	if err != nil {
		t.Fatalf("NewURLGuard(): %v", err)
	}
	connection, err := guard.DialContext(context.Background(), "tcp", "service.test:443")
	if err != nil {
		t.Fatalf("DialContext(): %v", err)
	}
	_ = connection.Close()
	want := []string{"93.184.216.34:443", "1.1.1.1:443"}
	if !reflect.DeepEqual(dialer.addresses, want) {
		t.Fatalf("dial addresses = %#v, want %#v", dialer.addresses, want)
	}
}

func TestURLGuardDialContextFailsClosed(t *testing.T) {
	t.Parallel()

	blocked, _ := NewURLGuard(URLGuardConfig{
		Resolver: fakeResolver{addresses: map[string][]netip.Addr{"service.test": {netip.MustParseAddr("127.0.0.1")}}},
		Dialer:   &fakeDialer{},
	})
	if _, err := blocked.DialContext(context.Background(), "tcp", "service.test:80"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("DialContext(blocked) error = %v", err)
	}
	if _, err := blocked.DialContext(context.Background(), "tcp", "invalid-address"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("DialContext(invalid) error = %v", err)
	}

	failing := &fakeDialer{failures: 2}
	guard, _ := NewURLGuard(URLGuardConfig{
		Resolver: fakeResolver{addresses: map[string][]netip.Addr{
			"service.test": {netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")},
		}},
		Dialer: failing,
	})
	if _, err := guard.DialContext(context.Background(), "tcp", "service.test:443"); err == nil {
		t.Fatal("DialContext(all failed) error = nil")
	}
	var nilGuard *URLGuard
	if _, err := nilGuard.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil DialContext() error = %v", err)
	}
}

func TestPublicAddressRejectsInvalidAddress(t *testing.T) {
	t.Parallel()
	if publicAddress(netip.Addr{}) {
		t.Fatal("publicAddress(invalid) = true")
	}
}

type fakeResolver struct {
	addresses map[string][]netip.Addr
	err       error
	lookup    func(context.Context) ([]netip.Addr, error)
}

func (resolver fakeResolver) LookupNetIP(ctx context.Context, _ string, host string) ([]netip.Addr, error) {
	if resolver.lookup != nil {
		return resolver.lookup(ctx)
	}
	if resolver.err != nil {
		return nil, resolver.err
	}
	return resolver.addresses[host], nil
}

type fakeDialer struct {
	addresses []string
	failures  int
}

func (dialer *fakeDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	dialer.addresses = append(dialer.addresses, address)
	if len(dialer.addresses) <= dialer.failures {
		return nil, errors.New("dial failed")
	}
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}
