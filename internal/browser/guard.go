package browser

import "github.com/Rememorio/gofer/internal/netguard"

var (
	// ErrUnsafeURL identifies a URL blocked by browser network policy.
	ErrUnsafeURL = netguard.ErrUnsafeURL
	// ErrInvalidConfig identifies malformed browser configuration.
	ErrInvalidConfig = netguard.ErrInvalidConfig
)

// Resolver is the DNS contract used by URLGuard.
type Resolver = netguard.Resolver

// URLGuardConfig configures browser URL validation.
type URLGuardConfig = netguard.URLGuardConfig

// URLGuard validates direct navigations and every browser network request.
type URLGuard = netguard.URLGuard

// NewURLGuard constructs a fail-closed browser URL guard.
func NewURLGuard(config URLGuardConfig) (*URLGuard, error) {
	return netguard.NewURLGuard(config)
}
