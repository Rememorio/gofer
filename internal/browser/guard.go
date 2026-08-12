package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrUnsafeURL identifies a URL blocked by browser network policy.
	ErrUnsafeURL = errors.New("unsafe browser URL")
	// ErrInvalidConfig identifies malformed browser configuration.
	ErrInvalidConfig = errors.New("invalid browser configuration")
)

var reservedPrefixes = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"2001:db8::/32",
)

// Resolver is the DNS contract used by URLGuard.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// URLGuardConfig configures browser URL validation.
type URLGuardConfig struct {
	AllowPrivateAddresses bool
	ResolveTimeout        time.Duration
	Resolver              Resolver
}

// URLGuard validates direct navigations and every browser network request.
type URLGuard struct {
	allowPrivate bool
	timeout      time.Duration
	resolver     Resolver
}

// NewURLGuard constructs a fail-closed browser URL guard.
func NewURLGuard(config URLGuardConfig) (*URLGuard, error) {
	if config.ResolveTimeout < 0 {
		return nil, fmt.Errorf("%w: resolve timeout must not be negative", ErrInvalidConfig)
	}
	if config.ResolveTimeout == 0 {
		config.ResolveTimeout = 2 * time.Second
	}
	if config.Resolver == nil {
		config.Resolver = net.DefaultResolver
	}
	return &URLGuard{
		allowPrivate: config.AllowPrivateAddresses,
		timeout:      config.ResolveTimeout, resolver: config.Resolver,
	}, nil
}

// ValidateNavigation accepts only public HTTP(S) URLs suitable for a top-level navigation.
func (guard *URLGuard) ValidateNavigation(ctx context.Context, rawURL string) (string, error) {
	parsed, err := guard.validateHTTP(ctx, rawURL)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// ValidateRequest validates browser-initiated requests, including redirects
// and subresources. Browser-local data, blob and about URLs are allowed.
func (guard *URLGuard) ValidateRequest(ctx context.Context, rawURL string) error {
	if guard == nil {
		return fmt.Errorf("%w: URL guard is nil", ErrInvalidConfig)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: parse URL: %w", ErrUnsafeURL, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "data", "blob", "about":
		return nil
	case "http", "https":
		_, err = guard.validateHTTP(ctx, rawURL)
		return err
	default:
		return fmt.Errorf("%w: scheme %q is not allowed", ErrUnsafeURL, parsed.Scheme)
	}
}

func (guard *URLGuard) validateHTTP(ctx context.Context, rawURL string) (*url.URL, error) {
	if guard == nil || guard.resolver == nil {
		return nil, fmt.Errorf("%w: URL guard is not configured", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%w: absolute HTTP(S) URL without credentials is required", ErrUnsafeURL)
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" {
		return nil, fmt.Errorf("%w: hostname is required", ErrUnsafeURL)
	}
	if guard.allowPrivate {
		return parsed, nil
	}
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return nil, fmt.Errorf("%w: local hostname is blocked", ErrUnsafeURL)
	}
	addresses, err := guard.resolve(ctx, hostname)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if !publicAddress(address) {
			return nil, fmt.Errorf("%w: host resolves to blocked address %s", ErrUnsafeURL, address)
		}
	}
	return parsed, nil
}

func (guard *URLGuard) resolve(ctx context.Context, hostname string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(hostname); err == nil {
		return []netip.Addr{address.Unmap()}, nil
	}
	resolveContext, cancel := context.WithTimeout(ctx, guard.timeout)
	defer cancel()
	addresses, err := guard.resolver.LookupNetIP(resolveContext, "ip", hostname)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve host %q: %w", ErrUnsafeURL, hostname, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w: host %q has no addresses", ErrUnsafeURL, hostname)
	}
	for index := range addresses {
		addresses[index] = addresses[index].Unmap()
	}
	return addresses, nil
}

func publicAddress(address netip.Addr) bool {
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() ||
		address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range reservedPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(values))
	for index, value := range values {
		prefixes[index] = netip.MustParsePrefix(value)
	}
	return prefixes
}
