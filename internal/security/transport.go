// Package security provides SSRF protection for outbound HTTP requests.
//
// Flow: API-002 (SSRF Protection)
//
// SafeTransport wraps http.Transport to enforce IP blocklists, preventing
// the Webhook Worker from reaching internal infrastructure such as AWS
// metadata service (169.254.169.254), localhost, or private network ranges.
package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"watchpoint/internal/types"
)

// dnsTimeout is the maximum time allowed for DNS resolution.
const dnsTimeout = 500 * time.Millisecond

// ErrSSRFBlocked is returned when a request targets a blocked IP range.
var ErrSSRFBlocked = errors.New("ssrf: request to blocked IP range")

// ErrSSRFDNSTimeout is returned when DNS resolution exceeds the timeout.
var ErrSSRFDNSTimeout = errors.New("ssrf: DNS resolution timeout")

// ErrSSRFTooManyRedirects is returned when the redirect limit is exceeded.
var ErrSSRFTooManyRedirects = errors.New("ssrf: too many redirects")

// ErrSSRFDNSFailed is returned when DNS resolution fails entirely.
var ErrSSRFDNSFailed = errors.New("ssrf: DNS resolution failed")

// blockedNets holds the parsed CIDR blocks. Initialized once via sync.Once.
var (
	blockedNets []*net.IPNet
	initOnce    sync.Once
	initErr     error
)

// initBlockedNets parses types.SSRFBlockedCIDRs into net.IPNet structures.
// This is called once on first use for efficient runtime lookups.
func initBlockedNets() {
	initOnce.Do(func() {
		blockedNets = make([]*net.IPNet, 0, len(types.SSRFBlockedCIDRs))
		for _, cidr := range types.SSRFBlockedCIDRs {
			_, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				initErr = fmt.Errorf("ssrf: failed to parse CIDR %q: %w", cidr, err)
				return
			}
			blockedNets = append(blockedNets, ipNet)
		}
	})
}

// isBlockedIP checks if the given IP falls within any blocked CIDR range.
func isBlockedIP(ip net.IP) bool {
	for _, ipNet := range blockedNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// Resolver abstracts DNS resolution for testability.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// netResolver wraps net.Resolver to satisfy the Resolver interface.
type netResolver struct {
	r *net.Resolver
}

func (nr *netResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return nr.r.LookupIPAddr(ctx, host)
}

// SafeTransport wraps http.Transport to enforce IP blocklists.
// It uses types.SSRFValidator from 01-foundation-types and validates all
// resolved IPs during connection establishment, ensuring that no request
// can reach internal infrastructure.
type SafeTransport struct {
	// Validator is the SSRF validation function from types.SSRFValidator.
	// Used for URL-level validation (e.g., pre-flight checks).
	// The transport-level IP validation uses the parsed CIDR blocks directly
	// for efficiency during dial.
	Validator types.SSRFValidator

	// Base is the underlying http.Transport used for actual connections.
	Base *http.Transport

	// Resolver is used for DNS lookups. If nil, net.DefaultResolver is used.
	// Exposed for testing.
	Resolver Resolver
}

// NewSafeTransport creates a SafeTransport wrapping the provided base transport.
// If base is nil, a default http.Transport is used.
func NewSafeTransport(base *http.Transport) (*SafeTransport, error) {
	initBlockedNets()
	if initErr != nil {
		return nil, fmt.Errorf("ssrf: initialization failed: %w", initErr)
	}

	if base == nil {
		base = &http.Transport{}
	}

	st := &SafeTransport{
		Validator: newIPValidator(),
		Base:      base,
	}

	// Override the DialContext on the base transport to inject IP validation.
	base.DialContext = st.safeDialContext

	return st, nil
}

// RoundTrip implements http.RoundTripper. It delegates to the base transport
// which has its DialContext overridden with SSRF validation.
func (st *SafeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return st.Base.RoundTrip(req)
}

// safeDialContext resolves the host to IP addresses, validates each against
// the blocked CIDR list, and only dials if all resolved IPs are safe.
func (st *SafeTransport) safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("ssrf: invalid address %q: %w", addr, err)
	}

	// If the host is already an IP literal, validate it directly.
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrSSRFBlocked, ip.String())
		}
		dialer := &net.Dialer{}
		return dialer.DialContext(ctx, network, addr)
	}

	// Resolve DNS with a strict timeout.
	resolver := st.getResolver()
	dnsCtx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()

	ips, err := resolver.LookupIPAddr(dnsCtx, host)
	if err != nil {
		if dnsCtx.Err() != nil {
			return nil, fmt.Errorf("%w: host %q", ErrSSRFDNSTimeout, host)
		}
		return nil, fmt.Errorf("%w: host %q: %v", ErrSSRFDNSFailed, host, err)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("%w: host %q resolved to no addresses", ErrSSRFDNSFailed, host)
	}

	// Validate ALL resolved IPs before connecting to any.
	// This prevents DNS rebinding attacks where one safe IP is mixed with
	// a private IP.
	for _, ipAddr := range ips {
		if isBlockedIP(ipAddr.IP) {
			return nil, fmt.Errorf("%w: %s (resolved from %s)", ErrSSRFBlocked, ipAddr.IP.String(), host)
		}
	}

	// All IPs are safe. Dial the first one.
	target := net.JoinHostPort(ips[0].IP.String(), port)
	dialer := &net.Dialer{}
	return dialer.DialContext(ctx, network, target)
}

// getResolver returns the configured Resolver, or the default net.Resolver.
func (st *SafeTransport) getResolver() Resolver {
	if st.Resolver != nil {
		return st.Resolver
	}
	return &netResolver{r: net.DefaultResolver}
}

// CheckRedirect returns an http.Client CheckRedirect function that validates
// redirect targets against the SSRF blocklist. It also enforces a maximum
// number of redirects.
//
// maxRedirects specifies the maximum number of redirects to follow.
// resolver is optional; if nil, net.DefaultResolver is used.
func CheckRedirect(maxRedirects int, resolver Resolver) func(req *http.Request, via []*http.Request) error {
	initBlockedNets()

	if resolver == nil {
		resolver = &netResolver{r: net.DefaultResolver}
	}

	return func(req *http.Request, via []*http.Request) error {
		// Enforce redirect limit.
		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: limit is %d", ErrSSRFTooManyRedirects, maxRedirects)
		}

		host := req.URL.Hostname()
		if host == "" {
			return fmt.Errorf("%w: redirect URL has no host", ErrSSRFBlocked)
		}

		// If the host is an IP literal, validate it directly.
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return fmt.Errorf("%w: redirect to %s", ErrSSRFBlocked, ip.String())
			}
			return nil
		}

		// Resolve DNS for the redirect target with strict timeout.
		dnsCtx, cancel := context.WithTimeout(req.Context(), dnsTimeout)
		defer cancel()

		ips, err := resolver.LookupIPAddr(dnsCtx, host)
		if err != nil {
			if dnsCtx.Err() != nil {
				return fmt.Errorf("%w: redirect host %q", ErrSSRFDNSTimeout, host)
			}
			return fmt.Errorf("%w: redirect host %q: %v", ErrSSRFDNSFailed, host, err)
		}

		// Validate all resolved IPs.
		for _, ipAddr := range ips {
			if isBlockedIP(ipAddr.IP) {
				return fmt.Errorf("%w: redirect to %s (resolved from %s)", ErrSSRFBlocked, ipAddr.IP.String(), host)
			}
		}

		return nil
	}
}

// NewSSRFValidator creates a types.SSRFValidator function that validates
// webhook URLs against the SSRF blocklist. This is used at webhook URL
// validation time (API layer) to provide early feedback.
//
// The validator resolves the URL's host to IP addresses and checks each
// against the blocked CIDR ranges.
func NewSSRFValidator() (types.SSRFValidator, error) {
	initBlockedNets()
	if initErr != nil {
		return nil, fmt.Errorf("ssrf: initialization failed: %w", initErr)
	}

	resolver := &netResolver{r: net.DefaultResolver}

	return func(urlStr string) error {
		host := extractHost(urlStr)
		if host == "" {
			return fmt.Errorf("%w: unable to extract host from URL", ErrSSRFBlocked)
		}

		// If the host is an IP literal, validate directly.
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return fmt.Errorf("%w: %s", ErrSSRFBlocked, ip.String())
			}
			return nil
		}

		// Resolve DNS with timeout.
		ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		defer cancel()

		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: host %q", ErrSSRFDNSTimeout, host)
			}
			return fmt.Errorf("%w: host %q: %v", ErrSSRFDNSFailed, host, err)
		}

		for _, ipAddr := range ips {
			if isBlockedIP(ipAddr.IP) {
				return fmt.Errorf("%w: %s (resolved from %s)", ErrSSRFBlocked, ipAddr.IP.String(), host)
			}
		}

		return nil
	}, nil
}

// NewSafeHTTPClient creates an http.Client configured with SafeTransport
// and SSRF-aware redirect checking. This is the primary entry point for
// the Webhook Worker.
func NewSafeHTTPClient(timeout time.Duration, maxRedirects int) (*http.Client, error) {
	transport, err := NewSafeTransport(nil)
	if err != nil {
		return nil, err
	}

	var resolver Resolver
	if transport.Resolver != nil {
		resolver = transport.Resolver
	}

	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: CheckRedirect(maxRedirects, resolver),
	}, nil
}

// newIPValidator creates a types.SSRFValidator that checks IP addresses
// embedded in URLs against the blocked CIDR ranges. This is the Validator
// stored on SafeTransport.
func newIPValidator() types.SSRFValidator {
	return func(urlStr string) error {
		host := extractHost(urlStr)
		if host == "" {
			return fmt.Errorf("%w: unable to extract host from URL", ErrSSRFBlocked)
		}

		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return fmt.Errorf("%w: %s", ErrSSRFBlocked, ip.String())
			}
		}

		return nil
	}
}

// extractHost parses the hostname from a URL string using net/url.
// Returns empty string if parsing fails.
func extractHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
