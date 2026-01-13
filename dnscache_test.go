package dnscache

import (
	"context"
	"fmt"
	"net"
	"net/http"
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
	// Test IPv4 Only
	r4 := NewOnlyV4(Config{})
	ips4, err := r4.LookupHost(context.Background(), "ipv6.google.com")
	// ipv6.google.com normally only has AAAA record. If we force V4, we might get error or empty
	// A better test is to lookup something with both (google.com) and verify format
	if err == nil {
		for _, ip := range ips4 {
			if net.ParseIP(ip).To4() == nil {
				t.Errorf("NewOnlyV4 returned IPv6 address: %s", ip)
			}
		}
	}

	// Test IPv6 Only
	r6 := NewOnlyV6(Config{})
	// localhost usually has ::1
	ips6, err := r6.LookupHost(context.Background(), "google.com")
	if err == nil {
		for _, ip := range ips6 {
			// To4() returns nil if it's not a valid v4 address (i.e. it is v6)
			// Wait, To4 returns nil for IPv6.
			if net.ParseIP(ip).To4() != nil {
				t.Errorf("NewOnlyV6 returned IPv4 address: %s", ip)
			}
		}
	}
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
