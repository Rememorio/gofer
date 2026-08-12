package netguard

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
	// ErrUnsafeURL identifies a URL or resolved address blocked by policy.
	ErrUnsafeURL = errors.New("unsafe network URL")
	// ErrInvalidConfig identifies malformed network-guard configuration.
	ErrInvalidConfig = errors.New("invalid network guard configuration")
)

var reservedPrefixes = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
	"2001:20::/28", "2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16",
)

// Resolver is the DNS contract used by URLGuard.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// Dialer opens a connection to one policy-approved resolved address.
type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// URLGuardConfig configures outbound URL validation and guarded dialing.
type URLGuardConfig struct {
	AllowPrivateAddresses bool
	ResolveTimeout        time.Duration
	Resolver              Resolver
	Dialer                Dialer
}

// URLGuard validates URLs and binds outbound dialing to the same address
// policy, closing DNS-rebinding and redirect gaps.
type URLGuard struct {
	allowPrivate bool
	timeout      time.Duration
	resolver     Resolver
	dialer       Dialer
}

// NewURLGuard constructs a fail-closed outbound URL guard.
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
	if config.Dialer == nil {
		config.Dialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	}
	return &URLGuard{
		allowPrivate: config.AllowPrivateAddresses, timeout: config.ResolveTimeout,
		resolver: config.Resolver, dialer: config.Dialer,
	}, nil
}

// ValidateNavigation accepts only policy-approved absolute HTTP(S) URLs.
func (guard *URLGuard) ValidateNavigation(ctx context.Context, rawURL string) (string, error) {
	parsed, err := guard.validateHTTP(ctx, rawURL)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

// ValidateRequest validates browser-style subrequests. Browser-local data,
// blob, and about URLs are allowed; every network URL is checked.
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

// DialContext resolves and validates a transport address immediately before
// connecting, then dials the approved IP directly.
func (guard *URLGuard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if guard == nil || guard.resolver == nil || guard.dialer == nil {
		return nil, fmt.Errorf("%w: URL guard is not configured", ErrInvalidConfig)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return nil, fmt.Errorf("%w: invalid dial address %q", ErrUnsafeURL, address)
	}
	addresses, err := guard.allowedAddresses(ctx, strings.TrimSuffix(strings.ToLower(host), "."))
	if err != nil {
		return nil, err
	}
	var dialErr error
	for _, resolved := range addresses {
		connection, err := guard.dialer.DialContext(ctx, network, net.JoinHostPort(resolved.String(), port))
		if err == nil {
			return connection, nil
		}
		dialErr = errors.Join(dialErr, err)
	}
	return nil, fmt.Errorf("dial approved addresses for %q: %w", host, dialErr)
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
	if _, err := guard.allowedAddresses(ctx, hostname); err != nil {
		return nil, err
	}
	return parsed, nil
}

func (guard *URLGuard) allowedAddresses(ctx context.Context, hostname string) ([]netip.Addr, error) {
	if !guard.allowPrivate && (hostname == "localhost" || strings.HasSuffix(hostname, ".localhost")) {
		return nil, fmt.Errorf("%w: local hostname is blocked", ErrUnsafeURL)
	}
	addresses, err := guard.resolve(ctx, hostname)
	if err != nil {
		return nil, err
	}
	if guard.allowPrivate {
		return addresses, nil
	}
	for _, address := range addresses {
		if !publicAddress(address) {
			return nil, fmt.Errorf("%w: host resolves to blocked address %s", ErrUnsafeURL, address)
		}
	}
	return addresses, nil
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
	resolved := make([]netip.Addr, len(addresses))
	for index, address := range addresses {
		resolved[index] = address.Unmap()
	}
	return resolved, nil
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
