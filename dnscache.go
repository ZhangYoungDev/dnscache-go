package dnscache

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// DialStrategy defines how the resolver iterates through IP addresses when dialing.
type DialStrategy int

const (
	// DialStrategyRandom shuffles IPs randomly for load balancing (default).
	DialStrategyRandom DialStrategy = iota
	// DialStrategySequential tries IPs in DNS response order.
	DialStrategySequential
	// DialStrategyRoundRobin rotates through IPs in order across calls.
	DialStrategyRoundRobin
)

// Config holds the configuration for the Resolver.
type Config struct {
	// Disabled controls whether the cache is disabled.
	// Default is false (cache enabled).
	// If true, the Resolver behaves as a pass-through proxy, directly querying the upstream.
	Disabled bool

	// OnCacheMiss is executed when a key is not found in the cache
	// and a query is sent to the upstream resolver.
	OnCacheMiss func(host string)

	// OnChange is executed when the resolved IPs for a host change.
	// It receives the host name and the new list of IP addresses.
	OnChange func(host string, ips []string)

	// CacheTTL is the duration for which the DNS records are cached.
	// Default is 1 minute if not specified.
	CacheTTL time.Duration

	// CacheFailTTL is the duration for which failed DNS lookups are cached.
	// Default is 1 second.
	CacheFailTTL time.Duration

	// EnableAutoRefresh controls whether to automatically refresh cached entries
	// in the background when they are close to expiring.
	EnableAutoRefresh bool
	// PersistOnFailure controls whether to return stale cache entries
	// when the upstream resolver fails.
	PersistOnFailure bool
	// CleanupInterval is the interval at which the cache is cleaned up.
	// If specified, a background goroutine will periodically remove unused cache entries.
	// You must call Stop() to stop the background goroutine.
	CleanupInterval time.Duration

	// DialStrategy controls how the resolver iterates through IP addresses when dialing.
	// Default is DialStrategyRandom.
	DialStrategy DialStrategy
}

// DNSResolver is the interface for the upstream DNS resolver.
type DNSResolver interface {
	LookupHost(ctx context.Context, host string) (addrs []string, err error)
	LookupAddr(ctx context.Context, addr string) (names []string, err error)
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// ResolverStats holds the runtime statistics of the resolver.
type ResolverStats struct {
	CacheHits   uint64
	CacheMisses uint64
}

// Resolver is a DNS resolver that caches responses.
type Resolver struct {
	upstream    DNSResolver
	dialer      *net.Dialer
	cache       Cache
	config      Config
	lookupGroup singleflight.Group
	stop        chan struct{}
	stopOnce    sync.Once

	stats   ResolverStats
	rrIndex uint32 // for round-robin strategy
}

// Stats returns the current statistics of the resolver.
func (r *Resolver) Stats() ResolverStats {
	return ResolverStats{
		CacheHits:   atomic.LoadUint64(&r.stats.CacheHits),
		CacheMisses: atomic.LoadUint64(&r.stats.CacheMisses),
	}
}

// New creates a new Resolver with the given configuration.
func New(config Config) *Resolver {
	if config.CacheTTL == 0 {
		config.CacheTTL = time.Minute
	}
	if config.CacheFailTTL == 0 {
		config.CacheFailTTL = 1 * time.Second
	}
	r := &Resolver{
		upstream: net.DefaultResolver,
		dialer:   &net.Dialer{},
		cache:    newMemoryCache(),
		config:   config,
		stop:     make(chan struct{}),
	}

	if config.CleanupInterval > 0 {
		go r.runCleanupLoop()
	}

	return r
}

// NewOnlyV4 creates a new Resolver that only resolves IPv4 addresses.
func NewOnlyV4(config Config) *Resolver {
	r := New(config)
	r.upstream = &ipVersionResolver{network: "ip4"}
	return r
}

// NewOnlyV6 creates a new Resolver that only resolves IPv6 addresses.
func NewOnlyV6(config Config) *Resolver {
	r := New(config)
	r.upstream = &ipVersionResolver{network: "ip6"}
	return r
}

// ipVersionResolver wraps net.DefaultResolver to enforce a specific IP version.
type ipVersionResolver struct {
	network string
}

func (r *ipVersionResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	// LookupHost doesn't support forcing network, so we use LookupIP and convert back to strings
	// This ensures we respect the IP version constraint at the DNS query level
	ips, err := net.DefaultResolver.LookupIP(ctx, r.network, host)
	if err != nil {
		return nil, err
	}
	strs := make([]string, len(ips))
	for i, ip := range ips {
		strs[i] = ip.String()
	}
	return strs, nil
}

func (r *ipVersionResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return net.DefaultResolver.LookupAddr(ctx, addr)
}

func (r *ipVersionResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	// Always enforce our IP version constraint, ignoring the requested network.
	return net.DefaultResolver.LookupIP(ctx, r.network, host)
}

// Refresh performs a cleanup of unused cache entries.
// It removes any entries that haven't been accessed since the last Refresh call.
func (r *Resolver) Refresh(clearUnused bool) {
	if clearUnused {
		r.cache.Prune()
	}
}

// Stop stops the background cleanup goroutine if one was started.
func (r *Resolver) Stop() {
	r.stopOnce.Do(func() {
		close(r.stop)
	})
}

func (r *Resolver) runCleanupLoop() {
	ticker := time.NewTicker(r.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			r.Refresh(true)
		case <-r.stop:
			return
		}
	}
}
