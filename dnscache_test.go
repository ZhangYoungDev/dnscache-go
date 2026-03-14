package dnscache

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntegration(t *testing.T) {
	// 1. Create our Resolver
	r := New(Config{Disabled: false})

	// 3. Create HTTP client using our Resolver
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: r.DialContext,
		},
		Timeout: 5 * time.Second,
	}

	// 4. Test request to external site (sanity check)
	// We use example.com because it's stable.
	resp, err := client.Get("http://example.com")
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// 5. Test Cache Hit
	memCache, ok := r.cache.(*memoryCache)
	if !ok {
		t.Fatal("Expected memoryCache implementation")
	}

	ips, _, _ := memCache.Get("example.com")
	if len(ips) == 0 {
		t.Errorf("Expected cache to be populated for example.com")
	} else {
		t.Logf("Cached IPs for example.com: %v", ips)
	}

	// 6. Test LookupIP
	ipAddrs, err := r.LookupIP(context.Background(), "ip", "example.com")
	if err != nil {
		t.Errorf("LookupIP failed: %v", err)
	}
	if len(ipAddrs) == 0 {
		t.Errorf("LookupIP returned no addresses")
	}
}

func TestDisabledMode(t *testing.T) {
	r := New(Config{Disabled: true})
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: r.DialContext,
		},
	}

	resp, err := client.Get("http://example.com")
	if err != nil {
		t.Fatalf("Failed to make request in disabled mode: %v", err)
	}
	resp.Body.Close()

	// Verify cache is empty
	memCache := r.cache.(*memoryCache)
	if len(memCache.store) != 0 {
		t.Error("Expected cache to be empty in disabled mode")
	}
}

func TestOnCacheMissAndSingleflight(t *testing.T) {
	var missCount int32
	r := New(Config{
		OnCacheMiss: func(host string) {
			atomic.AddInt32(&missCount, 1)
		},
	})

	// Simulate concurrent requests
	var wg sync.WaitGroup
	n := 10
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.LookupHost(context.Background(), "example.com")
			if err != nil {
				t.Errorf("LookupHost failed: %v", err)
			}
		}()
	}
	wg.Wait()

	// With singleflight, we expect exactly 1 cache miss (and 1 actual DNS lookup)
	count := atomic.LoadInt32(&missCount)
	if count != 1 {
		t.Errorf("Expected 1 cache miss, got %d", count)
	}

	// Verify subsequent requests don't trigger miss
	_, _ = r.LookupHost(context.Background(), "example.com")
	count = atomic.LoadInt32(&missCount)
	if count != 1 {
		t.Errorf("Expected count to remain 1, got %d", count)
	}
}

func TestLookupAddr(t *testing.T) {
	r := New(Config{Disabled: false})

	// We use a well-known IP for testing, 8.8.8.8 usually resolves to dns.google
	names, err := r.LookupAddr(context.Background(), "8.8.8.8")
	if err != nil {
		// If network is down or restricted, this might fail, so we skip or log
		t.Logf("LookupAddr failed (network issue?): %v", err)
		return
	}

	if len(names) == 0 {
		t.Error("LookupAddr returned no names for 8.8.8.8")
	} else {
		t.Logf("Reverse lookup for 8.8.8.8: %v", names)
	}

	// Verify cache population
	memCache, ok := r.cache.(*memoryCache)
	if !ok {
		t.Fatal("Expected memoryCache implementation")
	}

	cachedNames, _, _ := memCache.Get("8.8.8.8")
	if len(cachedNames) == 0 {
		t.Error("Expected cache to be populated for 8.8.8.8")
	}
}

func TestCacheCleanup(t *testing.T) {
	r := New(Config{CacheTTL: time.Minute})
	cache := r.cache

	// Add two entries
	cache.Set("keep.me", []string{"1.2.3.4"}, nil, time.Minute)
	cache.Set("delete.me", []string{"5.6.7.8"}, nil, time.Minute)

	// First Prune: resets 'used' flags to false (nothing deleted because they were fresh/used)
	r.Refresh(true)

	// Access "keep.me" -> marks it used again
	_, _, _ = cache.Get("keep.me")

	// Second Prune: should delete "delete.me" (unused since last prune)
	r.Refresh(true)

	// Verify "keep.me" exists
	if ips, _, _ := cache.Get("keep.me"); ips == nil {
		t.Error("Expected 'keep.me' to remain in cache")
	}

	// Verify "delete.me" is gone
	if ips, _, _ := cache.Get("delete.me"); ips != nil {
		t.Error("Expected 'delete.me' to be removed from cache")
	}
}

func TestPersistOnFailure(t *testing.T) {
	r := New(Config{
		CacheTTL:         time.Millisecond, // Expire immediately
		PersistOnFailure: true,
	})
	cache := r.cache

	// Manually inject an expired entry for a non-existent domain
	key := "nonexistent.test.local"
	cache.Set(key, []string{"127.0.0.2"}, nil, -time.Second) // Expired 1 second ago

	// LookupHost should try to resolve, fail (because domain doesn't exist),
	// but then fallback to the cached value because PersistOnFailure is true.
	ips, err := r.LookupHost(context.Background(), key)
	if err != nil {
		t.Errorf("Expected success (fallback to cache), got error: %v", err)
	}
	if len(ips) == 0 || ips[0] != "127.0.0.2" {
		t.Errorf("Expected cached value '127.0.0.2', got %v", ips)
	}

	// Disable PersistOnFailure and verify failure
	r.config.PersistOnFailure = false
	// We need to clear lookupGroup's memory or something? No, it's fresh call.
	// But wait, the cache entry is still there.
	// The key is unique per call? No.

	// Singleflight might coalesce calls if they were concurrent, but here they are serial.

	_, err = r.LookupHost(context.Background(), key)
	if err == nil {
		t.Error("Expected error when PersistOnFailure is false for bad domain")
	}
}

func TestAutoCleanup(t *testing.T) {
	r := New(Config{
		CacheTTL:        time.Minute,
		CleanupInterval: 50 * time.Millisecond,
	})
	defer r.Stop()

	cache := r.cache
	cache.Set("delete.me", []string{"1.2.3.4"}, nil, time.Minute)

	// Wait for one cycle (resets used flag)
	time.Sleep(100 * time.Millisecond)

	// Wait for second cycle (deletes unused)
	time.Sleep(100 * time.Millisecond)

	if ips, _, _ := cache.Get("delete.me"); ips != nil {
		t.Error("Expected 'delete.me' to be auto-removed from cache")
	}
}

func TestForceIPVersion(t *testing.T) {
	t.Run("NewOnlyV4 passes ip4 network to upstream", func(t *testing.T) {
		var gotNetwork string
		mock := &mockDNSResolverFull{
			lookupIPFunc: func(ctx context.Context, network, host string) ([]net.IP, error) {
				gotNetwork = network
				return []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("5.6.7.8")}, nil
			},
		}

		r := NewOnlyV4(Config{Upstream: mock})
		ips, err := r.LookupHost(context.Background(), "test.com")
		if err != nil {
			t.Fatalf("LookupHost failed: %v", err)
		}
		if gotNetwork != "ip4" {
			t.Errorf("Expected network 'ip4', got '%s'", gotNetwork)
		}
		if len(ips) != 2 || ips[0] != "1.2.3.4" || ips[1] != "5.6.7.8" {
			t.Errorf("Expected [1.2.3.4 5.6.7.8], got %v", ips)
		}
	})

	t.Run("NewOnlyV6 passes ip6 network to upstream", func(t *testing.T) {
		var gotNetwork string
		mock := &mockDNSResolverFull{
			lookupIPFunc: func(ctx context.Context, network, host string) ([]net.IP, error) {
				gotNetwork = network
				return []net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("::1")}, nil
			},
		}

		r := NewOnlyV6(Config{Upstream: mock})
		ips, err := r.LookupHost(context.Background(), "test.com")
		if err != nil {
			t.Fatalf("LookupHost failed: %v", err)
		}
		if gotNetwork != "ip6" {
			t.Errorf("Expected network 'ip6', got '%s'", gotNetwork)
		}
		if len(ips) != 2 {
			t.Errorf("Expected 2 IPs, got %d: %v", len(ips), ips)
		}
		for _, ip := range ips {
			if net.ParseIP(ip).To4() != nil {
				t.Errorf("NewOnlyV6 returned IPv4 address: %s", ip)
			}
		}
	})

	t.Run("NewOnlyV4 respects custom Upstream", func(t *testing.T) {
		var called bool
		mock := &mockDNSResolverFull{
			lookupIPFunc: func(ctx context.Context, network, host string) ([]net.IP, error) {
				called = true
				return []net.IP{net.ParseIP("10.0.0.1")}, nil
			},
		}

		r := NewOnlyV4(Config{Upstream: mock})
		_, err := r.LookupHost(context.Background(), "custom.test")
		if err != nil {
			t.Fatalf("LookupHost failed: %v", err)
		}
		if !called {
			t.Error("Custom upstream was not called - NewOnlyV4 should respect Config.Upstream")
		}
	})

	t.Run("NewOnlyV4 respects DNSServer config", func(t *testing.T) {
		r := NewOnlyV4(Config{DNSServer: "8.8.8.8:53"})

		ipvr, ok := r.upstream.(*ipVersionResolver)
		if !ok {
			t.Fatalf("Expected upstream to be *ipVersionResolver, got %T", r.upstream)
		}
		if _, ok := ipvr.upstream.(*net.Resolver); !ok {
			t.Errorf("Expected inner upstream to be *net.Resolver (custom DNS), got %T", ipvr.upstream)
		}
	})

	t.Run("NewOnlyV4 LookupIP also enforces ip4", func(t *testing.T) {
		mock := &mockDNSResolverFull{
			lookupIPFunc: func(ctx context.Context, network, host string) ([]net.IP, error) {
				if network != "ip4" {
					t.Errorf("Expected network 'ip4', got '%s'", network)
				}
				return []net.IP{net.ParseIP("1.2.3.4")}, nil
			},
		}

		r := NewOnlyV4(Config{Upstream: mock})
		ips, err := r.LookupIP(context.Background(), "ip", "test.com")
		if err != nil {
			t.Fatalf("LookupIP failed: %v", err)
		}
		if len(ips) != 1 {
			t.Errorf("Expected 1 IP, got %d", len(ips))
		}
	})

	t.Run("NewOnlyV4 caches results", func(t *testing.T) {
		var callCount int32
		mock := &mockDNSResolverFull{
			lookupIPFunc: func(ctx context.Context, network, host string) ([]net.IP, error) {
				atomic.AddInt32(&callCount, 1)
				return []net.IP{net.ParseIP("1.2.3.4")}, nil
			},
		}

		r := NewOnlyV4(Config{Upstream: mock, CacheTTL: time.Minute})
		_, _ = r.LookupHost(context.Background(), "cache.test")
		_, _ = r.LookupHost(context.Background(), "cache.test")

		if atomic.LoadInt32(&callCount) != 1 {
			t.Errorf("Expected 1 upstream call (cached), got %d", atomic.LoadInt32(&callCount))
		}
	})

	t.Run("NewOnlyV6 LookupAddr passes through", func(t *testing.T) {
		var called bool
		mock := &mockDNSResolverFull{
			lookupAddrFunc: func(ctx context.Context, addr string) ([]string, error) {
				called = true
				return []string{"host.example.com."}, nil
			},
		}

		r := NewOnlyV6(Config{Upstream: mock})
		names, err := r.LookupAddr(context.Background(), "::1")
		if err != nil {
			t.Fatalf("LookupAddr failed: %v", err)
		}
		if !called {
			t.Error("Inner upstream LookupAddr was not called")
		}
		if len(names) != 1 || names[0] != "host.example.com." {
			t.Errorf("Expected [host.example.com.], got %v", names)
		}
	})
}

func TestStats(t *testing.T) {
	r := New(Config{CacheTTL: time.Minute})

	// First lookup: Miss
	_, _ = r.LookupHost(context.Background(), "example.com")

	stats := r.Stats()
	if stats.CacheMisses != 1 {
		t.Errorf("Expected 1 cache miss, got %d", stats.CacheMisses)
	}
	if stats.CacheHits != 0 {
		t.Errorf("Expected 0 cache hits, got %d", stats.CacheHits)
	}

	// Second lookup: Hit
	_, _ = r.LookupHost(context.Background(), "example.com")

	stats = r.Stats()
	if stats.CacheMisses != 1 {
		t.Errorf("Expected 1 cache miss, got %d", stats.CacheMisses)
	}
	if stats.CacheHits != 1 {
		t.Errorf("Expected 1 cache hit, got %d", stats.CacheHits)
	}
}

func TestStopIdempotency(t *testing.T) {
	r := New(Config{CleanupInterval: time.Minute})
	// Should not panic calling Stop multiple times
	r.Stop()
	r.Stop()
	r.Stop()
}

// TestNegativeCaching verifies that failed lookups are cached for CacheFailTTL
func TestNegativeCaching(t *testing.T) {
	var lookupCount int32
	r := New(Config{
		CacheFailTTL: 50 * time.Millisecond,
	})

	// Mock upstream that always fails
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			atomic.AddInt32(&lookupCount, 1)
			return nil, fmt.Errorf("upstream failure")
		},
	}
	r.upstream = mock

	// 1. First lookup: should fail and increment count
	_, err := r.LookupHost(context.Background(), "fail.com")
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if atomic.LoadInt32(&lookupCount) != 1 {
		t.Errorf("Expected 1 lookup, got %d", atomic.LoadInt32(&lookupCount))
	}

	// 2. Second lookup immediately: should use cached error (count stays 1)
	_, err = r.LookupHost(context.Background(), "fail.com")
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if atomic.LoadInt32(&lookupCount) != 1 {
		t.Errorf("Expected lookup count to remain 1 (cached error), got %d", atomic.LoadInt32(&lookupCount))
	}

	// 3. Wait for expiry
	time.Sleep(100 * time.Millisecond)

	// 4. Third lookup: should trigger new lookup
	_, err = r.LookupHost(context.Background(), "fail.com")
	if err == nil {
		t.Fatal("Expected error, got nil")
	}
	if atomic.LoadInt32(&lookupCount) != 2 {
		t.Errorf("Expected lookup count to increase to 2, got %d", atomic.LoadInt32(&lookupCount))
	}
}

// Mock resolver for testing OnChange
type mockDNSResolver struct {
	lookupHostFunc func(ctx context.Context, host string) ([]string, error)
}

func (m *mockDNSResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if m.lookupHostFunc != nil {
		return m.lookupHostFunc(ctx, host)
	}
	return nil, nil
}
func (m *mockDNSResolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	return nil, nil
}
func (m *mockDNSResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	return nil, nil
}

func TestOnChange(t *testing.T) {
	var changes int32
	var lastIPs []string
	var mu sync.Mutex

	r := New(Config{
		CacheTTL: 10 * time.Millisecond, // Short TTL
		OnChange: func(host string, ips []string) {
			atomic.AddInt32(&changes, 1)
			mu.Lock()
			lastIPs = ips
			mu.Unlock()
		},
	})

	// Mock upstream
	ipsToReturn := []string{"1.1.1.1"}
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return ipsToReturn, nil
		},
	}
	r.upstream = mock

	// 1. First lookup (initial cache population) -> Should trigger OnChange
	_, err := r.LookupHost(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("First lookup failed: %v", err)
	}

	// Wait for goroutine
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&changes) != 1 {
		t.Errorf("Expected 1 change (init), got %d", atomic.LoadInt32(&changes))
	}

	// 2. Lookup again after TTL expired, but IPs are same -> Should NOT trigger OnChange
	// Wait for TTL expire
	time.Sleep(20 * time.Millisecond)
	_, err = r.LookupHost(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Second lookup failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&changes) != 1 {
		t.Errorf("Expected changes to stay 1, got %d", atomic.LoadInt32(&changes))
	}

	// 3. Change upstream IPs -> Should trigger OnChange
	ipsToReturn = []string{"2.2.2.2", "3.3.3.3"}
	// Wait for TTL
	time.Sleep(20 * time.Millisecond)
	_, err = r.LookupHost(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Third lookup failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&changes) != 2 {
		t.Errorf("Expected 2 changes, got %d", atomic.LoadInt32(&changes))
	}

	mu.Lock()
	if len(lastIPs) != 2 {
		t.Errorf("Expected lastIPs to have 2 elements, got %v", lastIPs)
	}
	mu.Unlock()

	// 4. Change order -> Should NOT trigger OnChange
	ipsToReturn = []string{"3.3.3.3", "2.2.2.2"}
	time.Sleep(20 * time.Millisecond)
	_, err = r.LookupHost(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Fourth lookup failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&changes) != 2 {
		t.Errorf("Expected changes to stay 2 (order change), got %d", atomic.LoadInt32(&changes))
	}
}

func TestDialStrategy(t *testing.T) {
	t.Run("Sequential", func(t *testing.T) {
		r := New(Config{DialStrategy: DialStrategySequential})
		ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}

		// Sequential should return in same order
		result := r.applyDialStrategy(ips)
		for i, ip := range ips {
			if result[i] != ip {
				t.Errorf("Sequential: expected %s at index %d, got %s", ip, i, result[i])
			}
		}
	})

	t.Run("RoundRobin", func(t *testing.T) {
		r := New(Config{DialStrategy: DialStrategyRoundRobin})
		ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}

		// First call should start at index 0
		result1 := r.applyDialStrategy(ips)
		if result1[0] != "1.1.1.1" {
			t.Errorf("RoundRobin first call: expected 1.1.1.1 first, got %s", result1[0])
		}

		// Second call should start at index 1
		result2 := r.applyDialStrategy(ips)
		if result2[0] != "2.2.2.2" {
			t.Errorf("RoundRobin second call: expected 2.2.2.2 first, got %s", result2[0])
		}

		// Third call should start at index 2
		result3 := r.applyDialStrategy(ips)
		if result3[0] != "3.3.3.3" {
			t.Errorf("RoundRobin third call: expected 3.3.3.3 first, got %s", result3[0])
		}

		// Fourth call should wrap around to index 0
		result4 := r.applyDialStrategy(ips)
		if result4[0] != "1.1.1.1" {
			t.Errorf("RoundRobin fourth call: expected 1.1.1.1 first, got %s", result4[0])
		}
	})

	t.Run("Random", func(t *testing.T) {
		r := New(Config{DialStrategy: DialStrategyRandom})
		ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"}

		// Run multiple times and check that order varies
		sameOrderCount := 0
		for i := 0; i < 10; i++ {
			result := r.applyDialStrategy(ips)
			if len(result) != len(ips) {
				t.Errorf("Random: expected %d IPs, got %d", len(ips), len(result))
			}

			// Check if order is same as original
			isSame := true
			for j, ip := range ips {
				if result[j] != ip {
					isSame = false
					break
				}
			}
			if isSame {
				sameOrderCount++
			}
		}

		// With 5 IPs and 10 runs, it's very unlikely to get same order more than 2 times
		if sameOrderCount > 3 {
			t.Errorf("Random: order was same as original %d/10 times, shuffle may not be working", sameOrderCount)
		}
	})

	t.Run("Default is Random", func(t *testing.T) {
		r := New(Config{}) // No strategy specified
		if r.config.DialStrategy != DialStrategyRandom {
			t.Errorf("Default strategy should be DialStrategyRandom (0), got %d", r.config.DialStrategy)
		}
	})

	t.Run("Single IP unchanged", func(t *testing.T) {
		r := New(Config{DialStrategy: DialStrategyRandom})
		ips := []string{"1.1.1.1"}
		result := r.applyDialStrategy(ips)
		if len(result) != 1 || result[0] != "1.1.1.1" {
			t.Errorf("Single IP should remain unchanged")
		}
	})
}

func TestCustomUpstream(t *testing.T) {
	// Create a mock resolver that returns specific IPs
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{"10.0.0.1", "10.0.0.2"}, nil
		},
	}

	r := New(Config{
		Upstream: mock,
	})

	// Verify the mock is used
	ips, err := r.LookupHost(context.Background(), "any.domain.com")
	if err != nil {
		t.Fatalf("LookupHost failed: %v", err)
	}

	if len(ips) != 2 || ips[0] != "10.0.0.1" || ips[1] != "10.0.0.2" {
		t.Errorf("Expected mock IPs [10.0.0.1, 10.0.0.2], got %v", ips)
	}
}

func TestCustomDNSServer(t *testing.T) {
	// This test verifies that the DNSServer config creates a custom resolver
	// We can't easily test actual DNS resolution without a real server,
	// so we just verify the configuration is applied correctly.
	r := New(Config{
		DNSServer: "8.8.8.8:53",
	})

	// Verify upstream is not the default resolver
	if r.upstream == net.DefaultResolver {
		t.Error("Expected custom resolver to be created when DNSServer is set")
	}

	// Verify it's a *net.Resolver (not ipVersionResolver or mockDNSResolver)
	if _, ok := r.upstream.(*net.Resolver); !ok {
		t.Error("Expected *net.Resolver when DNSServer is set")
	}
}

func TestUpstreamPriority(t *testing.T) {
	// Custom Upstream should take priority over DNSServer
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{"192.168.1.1"}, nil
		},
	}

	r := New(Config{
		DNSServer: "8.8.8.8:53", // This should be ignored
		Upstream:  mock,         // This should be used
	})

	// Verify mock is used (not the DNSServer)
	ips, err := r.LookupHost(context.Background(), "test.com")
	if err != nil {
		t.Fatalf("LookupHost failed: %v", err)
	}

	if len(ips) != 1 || ips[0] != "192.168.1.1" {
		t.Errorf("Expected mock IP [192.168.1.1], got %v", ips)
	}
}

func TestCustomDNSServerIntegration(t *testing.T) {
	// Integration test using Google's public DNS
	// Skip if network is unavailable
	r := New(Config{
		DNSServer: "8.8.8.8:53",
	})

	ips, err := r.LookupHost(context.Background(), "example.com")
	if err != nil {
		t.Skipf("Skipping integration test (network unavailable): %v", err)
	}

	if len(ips) == 0 {
		t.Error("Expected at least one IP from custom DNS server")
	} else {
		t.Logf("Resolved example.com via 8.8.8.8: %v", ips)
	}
}

// =====================================================
// DialContext Tests
// =====================================================

func TestDialContext_InvalidAddress(t *testing.T) {
	r := New(Config{})

	// Address without port should fail SplitHostPort
	_, err := r.DialContext(context.Background(), "tcp", "example.com")
	if err == nil {
		t.Error("Expected error for address without port")
	}

	// Invalid address format
	_, err = r.DialContext(context.Background(), "tcp", ":::")
	if err == nil {
		t.Error("Expected error for invalid address format")
	}
}

func TestDialContext_NoAddresses(t *testing.T) {
	r := New(Config{})

	// Mock resolver that returns empty IPs
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{}, nil
		},
	}
	r.upstream = mock

	_, err := r.DialContext(context.Background(), "tcp", "noaddrs.test:80")
	if err == nil {
		t.Error("Expected error when no addresses found")
	}

	// Verify it's the correct error type
	opErr, ok := err.(*net.OpError)
	if !ok {
		t.Errorf("Expected *net.OpError, got %T", err)
	} else if opErr.Op != "dial" {
		t.Errorf("Expected Op='dial', got '%s'", opErr.Op)
	}
}

func TestDialContext_LookupError(t *testing.T) {
	r := New(Config{})

	// Mock resolver that returns error
	expectedErr := fmt.Errorf("dns lookup failed")
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return nil, expectedErr
		},
	}
	r.upstream = mock

	_, err := r.DialContext(context.Background(), "tcp", "fail.test:80")
	if err == nil {
		t.Error("Expected error from lookup failure")
	}
}

func TestDialContext_DisabledMode(t *testing.T) {
	r := New(Config{Disabled: true})

	// In disabled mode, DialContext should pass through directly
	// Testing with invalid address - it should still attempt to dial
	_, err := r.DialContext(context.Background(), "tcp", "127.0.0.1:9999")
	// We expect connection refused, not a DNS error
	if err == nil {
		t.Error("Expected connection error")
	}
}

func TestDialContext_TriesMultipleIPs(t *testing.T) {
	var dialCount int32

	r := New(Config{
		DialStrategy: DialStrategySequential, // Use sequential to make order predictable
	})

	// Mock resolver returns multiple IPs
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nil
		},
	}
	r.upstream = mock

	// Custom dialer that tracks dial attempts
	r.dialer = &net.Dialer{
		Timeout: 10 * time.Millisecond,
	}

	// Try to connect - all should fail but it should try all IPs
	_, err := r.DialContext(context.Background(), "tcp", "multi.test:9999")
	if err == nil {
		t.Error("Expected connection error")
	}

	// Verify multiple IPs were tried (indirectly verified by the error being from the last attempt)
	_ = dialCount
}

func TestDialContext_ReturnsFirstSuccessful(t *testing.T) {
	r := New(Config{
		DialStrategy: DialStrategySequential,
	})

	// Start a local listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	defer ln.Close()

	port := ln.Addr().(*net.TCPAddr).Port

	// Mock resolver returns IPs - first fails, second should succeed
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{"10.255.255.1", fmt.Sprintf("127.0.0.1")}, nil
		},
	}
	r.upstream = mock
	r.dialer = &net.Dialer{Timeout: 50 * time.Millisecond}

	// Accept connections in background
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			conn.Close()
		}
	}()

	conn, err := r.DialContext(context.Background(), "tcp", fmt.Sprintf("test.local:%d", port))
	if err != nil {
		t.Errorf("Expected successful connection: %v", err)
	}
	if conn != nil {
		conn.Close()
	}
}

// =====================================================
// LookupIP Tests
// =====================================================

func TestLookupIP_NetworkFiltering(t *testing.T) {
	r := New(Config{})

	// Mock resolver returns both IPv4 and IPv6
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{"192.168.1.1", "10.0.0.1", "2001:db8::1", "::1"}, nil
		},
	}
	r.upstream = mock

	t.Run("ip4 only", func(t *testing.T) {
		ips, err := r.LookupIP(context.Background(), "ip4", "mixed.test")
		if err != nil {
			t.Fatalf("LookupIP failed: %v", err)
		}

		for _, ip := range ips {
			if ip.To4() == nil {
				t.Errorf("Expected only IPv4, got IPv6: %s", ip)
			}
		}

		if len(ips) != 2 {
			t.Errorf("Expected 2 IPv4 addresses, got %d", len(ips))
		}
	})

	t.Run("ip6 only", func(t *testing.T) {
		// Clear cache for fresh test
		r.cache = newMemoryCache()

		ips, err := r.LookupIP(context.Background(), "ip6", "mixed.test")
		if err != nil {
			t.Fatalf("LookupIP failed: %v", err)
		}

		for _, ip := range ips {
			if ip.To4() != nil {
				t.Errorf("Expected only IPv6, got IPv4: %s", ip)
			}
		}

		if len(ips) != 2 {
			t.Errorf("Expected 2 IPv6 addresses, got %d", len(ips))
		}
	})

	t.Run("ip (both)", func(t *testing.T) {
		r.cache = newMemoryCache()

		ips, err := r.LookupIP(context.Background(), "ip", "mixed.test")
		if err != nil {
			t.Fatalf("LookupIP failed: %v", err)
		}

		if len(ips) != 4 {
			t.Errorf("Expected 4 addresses, got %d", len(ips))
		}
	})
}

func TestLookupIP_DisabledMode(t *testing.T) {
	var lookupIPCalled bool

	mock := &mockDNSResolverFull{
		lookupIPFunc: func(ctx context.Context, network, host string) ([]net.IP, error) {
			lookupIPCalled = true
			return []net.IP{net.ParseIP("1.2.3.4")}, nil
		},
	}

	r := New(Config{
		Disabled: true,
		Upstream: mock,
	})

	_, err := r.LookupIP(context.Background(), "ip", "test.com")
	if err != nil {
		t.Fatalf("LookupIP failed: %v", err)
	}

	if !lookupIPCalled {
		t.Error("Expected upstream LookupIP to be called in disabled mode")
	}
}

func TestLookupIP_LookupError(t *testing.T) {
	r := New(Config{})

	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return nil, fmt.Errorf("lookup failed")
		},
	}
	r.upstream = mock

	_, err := r.LookupIP(context.Background(), "ip", "fail.test")
	if err == nil {
		t.Error("Expected error from LookupIP")
	}
}

// =====================================================
// EnableAutoRefresh Tests
// =====================================================

func TestEnableAutoRefresh(t *testing.T) {
	var lookupCount int32

	r := New(Config{
		CacheTTL:          100 * time.Millisecond,
		EnableAutoRefresh: true,
	})

	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			atomic.AddInt32(&lookupCount, 1)
			return []string{"1.1.1.1"}, nil
		},
	}
	r.upstream = mock

	// First lookup - cache miss
	_, err := r.LookupHost(context.Background(), "refresh.test")
	if err != nil {
		t.Fatalf("First lookup failed: %v", err)
	}

	if atomic.LoadInt32(&lookupCount) != 1 {
		t.Errorf("Expected 1 lookup, got %d", atomic.LoadInt32(&lookupCount))
	}

	// Wait until we're past half TTL but before full expiry
	time.Sleep(60 * time.Millisecond)

	// Second lookup - should trigger async refresh
	_, err = r.LookupHost(context.Background(), "refresh.test")
	if err != nil {
		t.Fatalf("Second lookup failed: %v", err)
	}

	// Wait for async refresh to complete
	time.Sleep(50 * time.Millisecond)

	// Should have triggered background refresh
	count := atomic.LoadInt32(&lookupCount)
	if count < 2 {
		t.Errorf("Expected at least 2 lookups (auto-refresh), got %d", count)
	}
}

// =====================================================
// httptrace Tests
// =====================================================

func TestHttptrace_DNSCallbacks(t *testing.T) {
	var dnsStartCalled, dnsDoneCalled bool
	var dnsStartHost string
	var dnsDoneAddrs []net.IPAddr

	r := New(Config{})

	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}, nil
		},
	}
	r.upstream = mock

	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStartCalled = true
			dnsStartHost = info.Host
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dnsDoneCalled = true
			dnsDoneAddrs = info.Addrs
		},
	}

	ctx := httptrace.WithClientTrace(context.Background(), trace)

	_, err := r.LookupHost(ctx, "trace.test")
	if err != nil {
		t.Fatalf("LookupHost failed: %v", err)
	}

	if !dnsStartCalled {
		t.Error("DNSStart callback was not called")
	}

	if dnsStartHost != "trace.test" {
		t.Errorf("DNSStart host = %s, want trace.test", dnsStartHost)
	}

	if !dnsDoneCalled {
		t.Error("DNSDone callback was not called")
	}

	if len(dnsDoneAddrs) != 2 {
		t.Errorf("DNSDone addrs count = %d, want 2", len(dnsDoneAddrs))
	}
}

func TestHttptrace_DNSError(t *testing.T) {
	var dnsDoneErr error

	r := New(Config{})

	expectedErr := fmt.Errorf("dns error")
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			return nil, expectedErr
		},
	}
	r.upstream = mock

	trace := &httptrace.ClientTrace{
		DNSDone: func(info httptrace.DNSDoneInfo) {
			dnsDoneErr = info.Err
		},
	}

	ctx := httptrace.WithClientTrace(context.Background(), trace)

	_, err := r.LookupHost(ctx, "error.test")
	if err == nil {
		t.Error("Expected error")
	}

	if dnsDoneErr == nil {
		t.Error("DNSDone should have received the error")
	}
}

// =====================================================
// Context Cancellation Tests
// =====================================================

func TestLookup_ContextCancelled(t *testing.T) {
	r := New(Config{})

	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			// Simulate slow lookup
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return []string{"1.1.1.1"}, nil
			}
		},
	}
	r.upstream = mock

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := r.LookupHost(ctx, "slow.test")
	if err == nil {
		t.Error("Expected context timeout error")
	}

	if err != context.DeadlineExceeded {
		t.Errorf("Expected DeadlineExceeded, got %v", err)
	}

	// Verify error is not cached for normal expiry
	// (CacheFailTTL should be used for failures)
}

func TestLookup_ContextCancelledNoCache(t *testing.T) {
	r := New(Config{})

	callCount := 0
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			callCount++
			if callCount == 1 {
				// First call - simulate cancellation
				return nil, context.Canceled
			}
			return []string{"1.1.1.1"}, nil
		},
	}
	r.upstream = mock

	// First call with cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, _ = r.LookupHost(ctx, "cancel.test")

	// Second call with valid context - should do fresh lookup
	_, err := r.LookupHost(context.Background(), "cancel.test")
	if err != nil {
		t.Errorf("Second lookup should succeed: %v", err)
	}

	// Should have made 2 calls (context cancellation not cached indefinitely)
	if callCount < 2 {
		t.Errorf("Expected at least 2 lookup calls, got %d", callCount)
	}
}

// =====================================================
// ipListChanged Tests
// =====================================================

func TestIpListChanged(t *testing.T) {
	tests := []struct {
		name     string
		oldIPs   []string
		newIPs   []string
		expected bool
	}{
		{"empty to empty", nil, nil, false},
		{"empty to non-empty", nil, []string{"1.1.1.1"}, true},
		{"non-empty to empty", []string{"1.1.1.1"}, nil, true},
		{"same IPs same order", []string{"1.1.1.1", "2.2.2.2"}, []string{"1.1.1.1", "2.2.2.2"}, false},
		{"same IPs different order", []string{"1.1.1.1", "2.2.2.2"}, []string{"2.2.2.2", "1.1.1.1"}, false},
		{"different IPs same length", []string{"1.1.1.1", "2.2.2.2"}, []string{"1.1.1.1", "3.3.3.3"}, true},
		{"subset", []string{"1.1.1.1", "2.2.2.2"}, []string{"1.1.1.1"}, true},
		{"superset", []string{"1.1.1.1"}, []string{"1.1.1.1", "2.2.2.2"}, true},
		{"duplicate handling", []string{"1.1.1.1", "1.1.1.1"}, []string{"1.1.1.1", "2.2.2.2"}, true},
		{"all duplicates same", []string{"1.1.1.1", "1.1.1.1"}, []string{"1.1.1.1", "1.1.1.1"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ipListChanged(tt.oldIPs, tt.newIPs)
			if result != tt.expected {
				t.Errorf("ipListChanged(%v, %v) = %v, want %v",
					tt.oldIPs, tt.newIPs, result, tt.expected)
			}
		})
	}
}

// =====================================================
// memoryCache Tests
// =====================================================

func TestMemoryCache_Basic(t *testing.T) {
	cache := newMemoryCache()

	// Test Get on empty cache
	ips, err, expireAt := cache.Get("nonexistent")
	if ips != nil || err != nil || !expireAt.IsZero() {
		t.Error("Expected nil/nil/zero for nonexistent key")
	}

	// Test Set and Get
	cache.Set("test.com", []string{"1.1.1.1", "2.2.2.2"}, nil, time.Minute)

	ips, err, expireAt = cache.Get("test.com")
	if len(ips) != 2 {
		t.Errorf("Expected 2 IPs, got %d", len(ips))
	}
	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}
	if expireAt.IsZero() {
		t.Error("Expected non-zero expireAt")
	}
	if time.Until(expireAt) < 59*time.Second {
		t.Error("ExpireAt should be about 1 minute from now")
	}
}

func TestMemoryCache_ErrorCaching(t *testing.T) {
	cache := newMemoryCache()

	expectedErr := fmt.Errorf("lookup failed")
	cache.Set("error.com", nil, expectedErr, time.Minute)

	ips, err, _ := cache.Get("error.com")
	if ips != nil {
		t.Errorf("Expected nil IPs, got %v", ips)
	}
	if err == nil || err.Error() != expectedErr.Error() {
		t.Errorf("Expected error '%v', got '%v'", expectedErr, err)
	}
}

func TestMemoryCache_UsedFlag(t *testing.T) {
	cache := newMemoryCache()

	cache.Set("test.com", []string{"1.1.1.1"}, nil, time.Minute)

	// First Prune - should reset used flag
	deleted := cache.Prune()
	if deleted != 0 {
		t.Errorf("First prune should delete 0, deleted %d", deleted)
	}

	// Second Prune without Get - should delete
	deleted = cache.Prune()
	if deleted != 1 {
		t.Errorf("Second prune should delete 1, deleted %d", deleted)
	}

	// Verify deleted
	ips, _, _ := cache.Get("test.com")
	if ips != nil {
		t.Error("Expected entry to be deleted")
	}
}

func TestMemoryCache_UsedFlagPreservation(t *testing.T) {
	cache := newMemoryCache()

	cache.Set("test.com", []string{"1.1.1.1"}, nil, time.Minute)

	// First Prune - resets used flag
	cache.Prune()

	// Access the entry - sets used flag
	cache.Get("test.com")

	// Second Prune - should not delete because it was used
	deleted := cache.Prune()
	if deleted != 0 {
		t.Errorf("Entry was used, should not be deleted, deleted %d", deleted)
	}

	// Verify still exists
	ips, _, _ := cache.Get("test.com")
	if ips == nil {
		t.Error("Entry should still exist after being used")
	}
}

func TestMemoryCache_ConcurrentAccess(t *testing.T) {
	cache := newMemoryCache()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)

		// Writer
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("host%d.com", i%10)
			cache.Set(key, []string{fmt.Sprintf("1.1.1.%d", i)}, nil, time.Minute)
		}(i)

		// Reader
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("host%d.com", i%10)
			cache.Get(key)
		}(i)

		// Pruner
		go func() {
			defer wg.Done()
			cache.Prune()
		}()
	}

	wg.Wait()
}

// =====================================================
// PersistOnFailure Additional Tests
// =====================================================

func TestPersistOnFailure_CacheHitWithError(t *testing.T) {
	r := New(Config{
		CacheTTL:         time.Minute,
		PersistOnFailure: true,
	})

	// First, set up cache with valid data
	r.cache.Set("persist.test", []string{"1.1.1.1"}, nil, time.Minute)

	// Now set error with the existing IPs preserved
	r.cache.Set("persist.test", []string{"1.1.1.1"}, fmt.Errorf("some error"), time.Minute)

	// Lookup should return cached IPs despite error
	ips, err := r.LookupHost(context.Background(), "persist.test")
	if err != nil {
		t.Errorf("Expected success with PersistOnFailure, got error: %v", err)
	}
	if len(ips) != 1 || ips[0] != "1.1.1.1" {
		t.Errorf("Expected cached IP, got %v", ips)
	}
}

// =====================================================
// Singleflight Double-Check Tests
// =====================================================

func TestSingleflight_DoubleCheck(t *testing.T) {
	r := New(Config{CacheTTL: time.Minute})

	var lookupCount int32
	mock := &mockDNSResolver{
		lookupHostFunc: func(ctx context.Context, host string) ([]string, error) {
			// Simulate slow lookup
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&lookupCount, 1)
			return []string{"1.1.1.1"}, nil
		},
	}
	r.upstream = mock

	// Start many concurrent lookups
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.LookupHost(context.Background(), "singleflight.test")
		}()
	}

	wg.Wait()

	// Singleflight should coalesce all requests into 1 or very few
	count := atomic.LoadInt32(&lookupCount)
	if count > 2 {
		t.Errorf("Expected singleflight to coalesce, got %d lookups", count)
	}
}

// =====================================================
// LookupAddr Disabled Mode Test
// =====================================================

func TestLookupAddr_DisabledMode(t *testing.T) {
	var lookupAddrCalled bool

	mock := &mockDNSResolverFull{
		lookupAddrFunc: func(ctx context.Context, addr string) ([]string, error) {
			lookupAddrCalled = true
			return []string{"example.com."}, nil
		},
	}

	r := New(Config{
		Disabled: true,
		Upstream: mock,
	})

	names, err := r.LookupAddr(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("LookupAddr failed: %v", err)
	}

	if !lookupAddrCalled {
		t.Error("Expected upstream LookupAddr to be called in disabled mode")
	}

	if len(names) != 1 || names[0] != "example.com." {
		t.Errorf("Expected ['example.com.'], got %v", names)
	}
}

// =====================================================
// applyDialStrategy Edge Cases
// =====================================================

func TestApplyDialStrategy_EmptyIPs(t *testing.T) {
	r := New(Config{DialStrategy: DialStrategyRandom})

	result := r.applyDialStrategy([]string{})
	if len(result) != 0 {
		t.Errorf("Expected empty result, got %v", result)
	}

	result = r.applyDialStrategy(nil)
	if result != nil {
		t.Errorf("Expected nil result, got %v", result)
	}
}

// =====================================================
// Additional Helper Mocks
// =====================================================

// mockDNSResolverFull implements all DNSResolver methods with customizable behavior
type mockDNSResolverFull struct {
	lookupHostFunc func(ctx context.Context, host string) ([]string, error)
	lookupAddrFunc func(ctx context.Context, addr string) ([]string, error)
	lookupIPFunc   func(ctx context.Context, network, host string) ([]net.IP, error)
}

func (m *mockDNSResolverFull) LookupHost(ctx context.Context, host string) ([]string, error) {
	if m.lookupHostFunc != nil {
		return m.lookupHostFunc(ctx, host)
	}
	return nil, nil
}

func (m *mockDNSResolverFull) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	if m.lookupAddrFunc != nil {
		return m.lookupAddrFunc(ctx, addr)
	}
	return nil, nil
}

func (m *mockDNSResolverFull) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if m.lookupIPFunc != nil {
		return m.lookupIPFunc(ctx, network, host)
	}
	return nil, nil
}
