package browser

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestURLGuardAcceptsPublicHTTP(t *testing.T) {
	t.Parallel()

	resolver := fakeResolver{addresses: map[string][]netip.Addr{
		"example.com": {netip.MustParseAddr("93.184.216.34")},
	}}
	guard, err := NewURLGuard(URLGuardConfig{Resolver: resolver})
	if err != nil {
		t.Fatalf("NewURLGuard(): %v", err)
	}
	validated, err := guard.ValidateNavigation(context.Background(), "https://example.com/path?q=1#fragment")
	if err != nil || validated != "https://example.com/path?q=1#fragment" {
		t.Fatalf("ValidateNavigation() = %q, %v", validated, err)
	}
	for _, rawURL := range []string{"data:text/plain,ok", "blob:https://example.com/id", "about:blank"} {
		if err := guard.ValidateRequest(context.Background(), rawURL); err != nil {
			t.Fatalf("ValidateRequest(%q): %v", rawURL, err)
		}
	}
	if !reflect.DeepEqual(resolver.addresses["example.com"], []netip.Addr{netip.MustParseAddr("93.184.216.34")}) {
		t.Fatal("resolver fixture was mutated")
	}
}

func TestURLGuardBlocksUnsafeAddresses(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254",
		"100.64.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "240.0.0.1", "::1", "fe80::1", "fc00::1", "2001:db8::1",
	}
	for _, address := range blocked {
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

func TestURLGuardRejectsMalformedAndUnresolvableURLs(t *testing.T) {
	t.Parallel()

	resolveError := errors.New("DNS unavailable")
	guard, _ := NewURLGuard(URLGuardConfig{Resolver: fakeResolver{err: resolveError}})
	invalid := []string{
		"", "relative", "file:///etc/passwd", "ftp://example.com", "https://user@example.com",
		"http://localhost", "http://service.localhost", "http://missing.test",
	}
	for _, rawURL := range invalid {
		if _, err := guard.ValidateNavigation(context.Background(), rawURL); !errors.Is(err, ErrUnsafeURL) {
			t.Fatalf("ValidateNavigation(%q) error = %v", rawURL, err)
		}
	}
	if err := guard.ValidateRequest(context.Background(), "file:///etc/passwd"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("ValidateRequest(file) error = %v", err)
	}
	var nilGuard *URLGuard
	if err := nilGuard.ValidateRequest(context.Background(), "https://example.com"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil ValidateRequest() error = %v", err)
	}
}

func TestURLGuardPrivateOptInAndContext(t *testing.T) {
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
}

func TestURLGuardResolutionTimeoutAndEmptyResult(t *testing.T) {
	t.Parallel()

	guard, _ := NewURLGuard(URLGuardConfig{
		ResolveTimeout: time.Millisecond,
		Resolver: fakeResolver{lookup: func(ctx context.Context) ([]netip.Addr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}},
	})
	if _, err := guard.ValidateNavigation(context.Background(), "https://slow.test"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("ValidateNavigation(timeout) error = %v", err)
	}
	guard, _ = NewURLGuard(URLGuardConfig{Resolver: fakeResolver{addresses: map[string][]netip.Addr{"empty.test": {}}}})
	if _, err := guard.ValidateNavigation(context.Background(), "https://empty.test"); !errors.Is(err, ErrUnsafeURL) {
		t.Fatalf("ValidateNavigation(empty) error = %v", err)
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
	return append([]netip.Addr(nil), resolver.addresses[host]...), nil
}
