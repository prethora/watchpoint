package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockResolver implements Resolver for deterministic testing.
type mockResolver struct {
	ips map[string][]net.IPAddr
	err error
}

func (m *mockResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if m.err != nil {
		return nil, m.err
	}
	ips, ok := m.ips[host]
	if !ok {
		return nil, fmt.Errorf("no such host: %s", host)
	}
	return ips, nil
}

// slowResolver simulates a DNS resolver that takes too long.
type slowResolver struct {
	delay time.Duration
}

func (s *slowResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	select {
	case <-time.After(s.delay):
		return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newMockResolver(mappings map[string]string) *mockResolver {
	ips := make(map[string][]net.IPAddr)
	for host, ipStr := range mappings {
		ips[host] = []net.IPAddr{{IP: net.ParseIP(ipStr)}}
	}
	return &mockResolver{ips: ips}
}

func newMultiMockResolver(mappings map[string][]string) *mockResolver {
	ips := make(map[string][]net.IPAddr)
	for host, ipStrs := range mappings {
		addrs := make([]net.IPAddr, len(ipStrs))
		for i, ipStr := range ipStrs {
			addrs[i] = net.IPAddr{IP: net.ParseIP(ipStr)}
		}
		ips[host] = addrs
	}
	return &mockResolver{ips: ips}
}

// TestInitBlockedNets verifies that all CIDR blocks from types.SSRFBlockedCIDRs
// parse correctly.
func TestInitBlockedNets(t *testing.T) {
	initBlockedNets()
	require.NoError(t, initErr, "blocked CIDRs should parse without error")
	require.NotEmpty(t, blockedNets, "blocked nets should not be empty")
}

// TestIsBlockedIP_Localhost tests that localhost IPs are blocked.
func TestIsBlockedIP_Localhost(t *testing.T) {
	initBlockedNets()
	require.NoError(t, initErr)

	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"127.0.0.1", "127.0.0.1", true},
		{"127.0.0.2", "127.0.0.2", true},
		{"127.255.255.255", "127.255.255.255", true},
		{"::1", "::1", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "failed to parse IP: %s", tt.ip)
			assert.Equal(t, tt.blocked, isBlockedIP(ip), "IP %s", tt.ip)
		})
	}
}

// TestIsBlockedIP_PrivateRanges tests that all private IP ranges are blocked.
func TestIsBlockedIP_PrivateRanges(t *testing.T) {
	initBlockedNets()
	require.NoError(t, initErr)

	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// Class A private
		{"10.0.0.1", "10.0.0.1", true},
		{"10.255.255.255", "10.255.255.255", true},

		// Class B private
		{"172.16.0.1", "172.16.0.1", true},
		{"172.31.255.255", "172.31.255.255", true},
		{"172.15.255.255", "172.15.255.255", false}, // Just below range
		{"172.32.0.0", "172.32.0.0", false},         // Just above range

		// Class C private
		{"192.168.0.1", "192.168.0.1", true},
		{"192.168.255.255", "192.168.255.255", true},

		// Link-local / AWS metadata
		{"169.254.169.254", "169.254.169.254", true},
		{"169.254.0.1", "169.254.0.1", true},

		// Current network
		{"0.0.0.0", "0.0.0.0", true},
		{"0.255.255.255", "0.255.255.255", true},

		// Multicast
		{"224.0.0.1", "224.0.0.1", true},
		{"239.255.255.255", "239.255.255.255", true},

		// Reserved
		{"240.0.0.1", "240.0.0.1", true},

		// Shared Address Space (CGN)
		{"100.64.0.1", "100.64.0.1", true},
		{"100.127.255.255", "100.127.255.255", true},

		// Benchmark testing
		{"198.18.0.1", "198.18.0.1", true},
		{"198.19.255.255", "198.19.255.255", true},

		// Public IPs should NOT be blocked
		{"8.8.8.8", "8.8.8.8", false},
		{"93.184.216.34", "93.184.216.34", false},
		{"1.1.1.1", "1.1.1.1", false},
		{"203.0.113.1", "203.0.113.1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "failed to parse IP: %s", tt.ip)
			assert.Equal(t, tt.blocked, isBlockedIP(ip), "IP %s blocked=%v", tt.ip, tt.blocked)
		})
	}
}

// TestIsBlockedIP_IPv6 tests that IPv6 private/link-local addresses are blocked.
func TestIsBlockedIP_IPv6(t *testing.T) {
	initBlockedNets()
	require.NoError(t, initErr)

	tests := []struct {
		name    string
		ip      string
		blocked bool
	}{
		{"IPv6 localhost", "::1", true},
		{"IPv6 private fc00", "fc00::1", true},
		{"IPv6 private fd00", "fd00::1", true},
		{"IPv6 link-local", "fe80::1", true},
		{"IPv6 public", "2001:db8::1", false},
		{"IPv6 global", "2607:f8b0:4004:800::200e", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "failed to parse IP: %s", tt.ip)
			assert.Equal(t, tt.blocked, isBlockedIP(ip), "IPv6 %s", tt.ip)
		})
	}
}

// TestSafeTransport_BlocksLocalhost verifies that SafeTransport blocks connections
// to localhost via DNS resolution.
func TestSafeTransport_BlocksLocalhost(t *testing.T) {
	resolver := newMockResolver(map[string]string{
		"evil.example.com": "127.0.0.1",
	})

	transport, err := NewSafeTransport(nil)
	require.NoError(t, err)
	transport.Resolver = resolver

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	_, err = client.Get("http://evil.example.com/webhook")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
		"expected SSRF blocked error, got: %v", err)
}

// TestSafeTransport_BlocksPrivateIP verifies that SafeTransport blocks connections
// to private IP addresses via DNS resolution.
func TestSafeTransport_BlocksPrivateIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{"Class A", "10.0.0.1"},
		{"Class B", "172.16.0.1"},
		{"Class C", "192.168.1.1"},
		{"AWS Metadata", "169.254.169.254"},
		{"CGN", "100.64.0.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := newMockResolver(map[string]string{
				"target.example.com": tt.ip,
			})

			transport, err := NewSafeTransport(nil)
			require.NoError(t, err)
			transport.Resolver = resolver

			client := &http.Client{
				Transport: transport,
				Timeout:   5 * time.Second,
			}

			_, err = client.Get("http://target.example.com/hook")
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
				"expected SSRF blocked error for %s, got: %v", tt.ip, err)
		})
	}
}

// TestSafeTransport_BlocksIPLiteral verifies that SafeTransport blocks
// direct IP literal access to blocked ranges.
func TestSafeTransport_BlocksIPLiteral(t *testing.T) {
	transport, err := NewSafeTransport(nil)
	require.NoError(t, err)

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	tests := []struct {
		name string
		url  string
	}{
		{"localhost", "http://127.0.0.1/webhook"},
		{"private", "http://10.0.0.1/webhook"},
		{"metadata", "http://169.254.169.254/latest/meta-data/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.Get(tt.url)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
				"expected SSRF blocked error for %s, got: %v", tt.url, err)
		})
	}
}

// TestSafeTransport_AllowsPublicIP verifies that SafeTransport allows connections
// to public IPs. We use a local httptest server as a stand-in for a public IP.
func TestSafeTransport_AllowsPublicIP(t *testing.T) {
	// Start a local test server.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	// The test server listens on 127.0.0.1 which is blocked, so we need
	// to use a mock resolver that maps a hostname to a public IP, and then
	// actually connect to the test server. Instead, let's verify the
	// resolver logic directly with a safe IP.
	resolver := newMockResolver(map[string]string{
		"safe.example.com": "93.184.216.34",
	})

	transport, err := NewSafeTransport(nil)
	require.NoError(t, err)
	transport.Resolver = resolver

	// The connection will fail (no actual server at 93.184.216.34:80 in test),
	// but the error should NOT be an SSRF error -- it should be a connection error.
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
	}

	_, err = client.Get("http://safe.example.com/webhook")
	// We expect a connection error (dial tcp timeout or refused), not an SSRF error.
	if err != nil {
		assert.False(t, errors.Is(err, ErrSSRFBlocked),
			"should not be SSRF blocked for public IP, got: %v", err)
		assert.NotContains(t, err.Error(), "ssrf: request to blocked IP range",
			"should not contain SSRF blocked message for public IP")
	}
}

// TestSafeTransport_BlocksMixedIPs verifies that when DNS resolves to both
// safe and unsafe IPs, the connection is blocked.
func TestSafeTransport_BlocksMixedIPs(t *testing.T) {
	resolver := newMultiMockResolver(map[string][]string{
		"mixed.example.com": {"93.184.216.34", "10.0.0.1"},
	})

	transport, err := NewSafeTransport(nil)
	require.NoError(t, err)
	transport.Resolver = resolver

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	_, err = client.Get("http://mixed.example.com/webhook")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
		"expected SSRF blocked error for mixed IPs, got: %v", err)
}

// TestSafeTransport_DNSTimeout verifies that DNS timeouts result in a fail-closed response.
func TestSafeTransport_DNSTimeout(t *testing.T) {
	resolver := &slowResolver{delay: 2 * time.Second}

	transport, err := NewSafeTransport(nil)
	require.NoError(t, err)
	transport.Resolver = resolver

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	_, err = client.Get("http://slow-dns.example.com/webhook")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFDNSTimeout) || strings.Contains(err.Error(), "DNS resolution timeout"),
		"expected DNS timeout error, got: %v", err)
}

// TestCheckRedirect_BlocksPrivateIP verifies that the CheckRedirect function
// blocks redirects to private IP addresses.
func TestCheckRedirect_BlocksPrivateIP(t *testing.T) {
	resolver := newMockResolver(map[string]string{
		"internal.example.com": "192.168.1.1",
	})

	checkFn := CheckRedirect(3, resolver)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://internal.example.com/hook", nil)
	require.NoError(t, err)

	via := []*http.Request{{}} // One previous redirect
	err = checkFn(req, via)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
		"expected SSRF blocked error on redirect, got: %v", err)
}

// TestCheckRedirect_BlocksLocalhostRedirect verifies that redirects to localhost are blocked.
func TestCheckRedirect_BlocksLocalhostRedirect(t *testing.T) {
	resolver := newMockResolver(map[string]string{
		"localhost.example.com": "127.0.0.1",
	})

	checkFn := CheckRedirect(3, resolver)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://localhost.example.com/hook", nil)
	require.NoError(t, err)

	err = checkFn(req, []*http.Request{{}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
		"expected SSRF blocked on redirect to localhost, got: %v", err)
}

// TestCheckRedirect_BlocksMetadataRedirect verifies that redirects to AWS
// metadata service IP are blocked.
func TestCheckRedirect_BlocksMetadataRedirect(t *testing.T) {
	checkFn := CheckRedirect(3, nil)

	// Direct IP literal in redirect URL.
	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://169.254.169.254/latest/meta-data/", nil)
	require.NoError(t, err)

	err = checkFn(req, []*http.Request{{}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFBlocked) || strings.Contains(err.Error(), "ssrf: request to blocked IP range"),
		"expected SSRF blocked on redirect to metadata, got: %v", err)
}

// TestCheckRedirect_AllowsPublicIP verifies that redirects to public IPs are allowed.
func TestCheckRedirect_AllowsPublicIP(t *testing.T) {
	resolver := newMockResolver(map[string]string{
		"safe.example.com": "93.184.216.34",
	})

	checkFn := CheckRedirect(3, resolver)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://safe.example.com/hook", nil)
	require.NoError(t, err)

	err = checkFn(req, []*http.Request{{}})
	assert.NoError(t, err, "redirect to public IP should be allowed")
}

// TestCheckRedirect_EnforcesMaxRedirects verifies the redirect count limit.
func TestCheckRedirect_EnforcesMaxRedirects(t *testing.T) {
	resolver := newMockResolver(map[string]string{
		"safe.example.com": "93.184.216.34",
	})

	maxRedirects := 3
	checkFn := CheckRedirect(maxRedirects, resolver)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://safe.example.com/hook", nil)
	require.NoError(t, err)

	// Create a via slice at the limit.
	via := make([]*http.Request, maxRedirects)
	for i := range via {
		via[i] = &http.Request{}
	}

	err = checkFn(req, via)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFTooManyRedirects) || strings.Contains(err.Error(), "too many redirects"),
		"expected too many redirects error, got: %v", err)
}

// TestCheckRedirect_AllowsWithinLimit verifies redirects within the limit are allowed.
func TestCheckRedirect_AllowsWithinLimit(t *testing.T) {
	resolver := newMockResolver(map[string]string{
		"safe.example.com": "93.184.216.34",
	})

	checkFn := CheckRedirect(3, resolver)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://safe.example.com/hook", nil)
	require.NoError(t, err)

	// Two previous redirects (within limit of 3).
	via := []*http.Request{{}, {}}
	err = checkFn(req, via)
	assert.NoError(t, err, "redirect within limit should be allowed")
}

// TestNewSafeHTTPClient verifies the factory creates a properly configured client.
func TestNewSafeHTTPClient(t *testing.T) {
	client, err := NewSafeHTTPClient(10*time.Second, 3)
	require.NoError(t, err)
	assert.NotNil(t, client)
	assert.Equal(t, 10*time.Second, client.Timeout)
	assert.NotNil(t, client.CheckRedirect)
	assert.NotNil(t, client.Transport)

	_, ok := client.Transport.(*SafeTransport)
	assert.True(t, ok, "transport should be *SafeTransport")
}

// TestNewSSRFValidator verifies the SSRFValidator function works correctly.
func TestNewSSRFValidator(t *testing.T) {
	validator, err := NewSSRFValidator()
	require.NoError(t, err)
	require.NotNil(t, validator)

	// Test with IP literal that is blocked.
	err = validator("https://127.0.0.1/webhook")
	assert.Error(t, err, "should block localhost URL")

	err = validator("https://169.254.169.254/latest/meta-data/")
	assert.Error(t, err, "should block metadata URL")

	err = validator("https://10.0.0.1/webhook")
	assert.Error(t, err, "should block private IP URL")
}

// TestExtractHost verifies the host extraction utility function.
func TestExtractHost(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{"HTTPS with port", "https://example.com:443/path", "example.com"},
		{"HTTPS without port", "https://example.com/path", "example.com"},
		{"HTTP with port", "http://example.com:8080/path?q=1", "example.com"},
		{"IP literal", "https://192.168.1.1/path", "192.168.1.1"},
		{"IP with port", "https://192.168.1.1:443/path", "192.168.1.1"},
		{"No scheme (invalid URL)", "example.com/path", ""},
		{"With userinfo", "https://user:pass@example.com/path", "example.com"},
		{"IPv6 literal", "https://[::1]:443/path", "::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHost(tt.url)
			assert.Equal(t, tt.expected, got, "extractHost(%q)", tt.url)
		})
	}
}

// TestSafeTransport_DNSResolutionFailure verifies fail-closed behavior on DNS errors.
func TestSafeTransport_DNSResolutionFailure(t *testing.T) {
	resolver := &mockResolver{
		err: errors.New("dns server unreachable"),
	}

	transport, err := NewSafeTransport(nil)
	require.NoError(t, err)
	transport.Resolver = resolver

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	_, err = client.Get("http://failing-dns.example.com/webhook")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFDNSFailed) || strings.Contains(err.Error(), "DNS resolution failed"),
		"expected DNS failure error, got: %v", err)
}

// TestCheckRedirect_DNSTimeout verifies that slow DNS during redirect validation
// results in fail-closed behavior.
func TestCheckRedirect_DNSTimeout(t *testing.T) {
	resolver := &slowResolver{delay: 2 * time.Second}
	checkFn := CheckRedirect(3, resolver)

	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://slow.example.com/hook", nil)
	require.NoError(t, err)

	err = checkFn(req, []*http.Request{{}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSSRFDNSTimeout) || strings.Contains(err.Error(), "DNS resolution timeout"),
		"expected DNS timeout error on redirect, got: %v", err)
}
