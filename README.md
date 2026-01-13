# dnscache-go

A production-ready DNS cache library for Go that integrates seamlessly with `net/http` and standard library network interfaces.

It is designed to eliminate DNS latency spikes and provide robust connection handling out of the box.

## Why dnscache-go?

While heavily inspired by existing solutions like `rs/dnscache`, this library introduces several key engineering improvements for production environments:

- **Smart Async Refresh (Opt-in)**: Unlike traditional caches that expire and block, this library can automatically refresh "hot" domains in the background when their TTL is halfway through. Enable `EnableAutoRefresh` to ensure your application **always hits the cache (0ms latency)** and never waits for DNS resolution during high traffic.
- **Negative Caching**: Automatically caches DNS failures for a short duration (default 1 second). This prevents "cache stampede" on the upstream DNS server during outages while ensuring rapid recovery when the service is restored.
- **Standard `DialContext` with Failover**: Directly implements the standard `DialContext` interface, making it a true drop-in replacement for `http.Transport`. It also includes built-in connection failover (Happy Eyeballs simplified), automatically retrying the next IP if the first one fails.
- **Configurable TTL**: Supports explicit Cache TTL configuration. Entries expire deterministically, and a background cleaner (optional) ensures unused entries are removed, preventing memory leaks without relying on manual refresh triggers.
- **Proper Context Handling**: Fully respects Go's `context` propagation, including cancellation and deadlines, without breaking the call chain.

## Features

- **Drop-in Replacement**: Implements `DialContext` for easy integration with `http.Transport`.
- **Cache Stampede Protection**: Uses `singleflight` to merge concurrent DNS queries.
- **Zero Latency**: Async auto-refresh (when enabled) keeps the cache warm for active domains.
- **High Availability**: Connection failover ensures robustness against single IP failures.
- **Observability**: Built-in metrics (`CacheHits`/`CacheMisses`), `OnCacheMiss` hook, and `httptrace` support.
- **Dynamic Updates**: `OnChange` hook allows reacting to IP changes in real-time (e.g., for service discovery).
- **Trace Support**: Respects `httptrace.ClientTrace` context for detailed performance analysis.
- **Custom DNS Server**: Supports custom DNS server addresses (e.g., `8.8.8.8:53`, `1.1.1.1:53`) to bypass system DNS.
- **Pluggable Upstream**: Allows custom `DNSResolver` implementations for DoH (DNS over HTTPS), DoT (DNS over TLS), or any other DNS protocol.
- **Zero Config**: Works out of the box with sensible defaults.

## Installation

```bash
go get github.com/ZhangYoungDev/dnscache-go
```

## Usage

### With `http.Client` (Default)

This is the most common use case. By replacing the `DialContext` in `http.Transport`, all requests made by the client will automatically use the DNS cache.

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ZhangYoungDev/dnscache-go"
)

func main() {
	resolver := dnscache.New(dnscache.Config{})

	transport := &http.Transport{
		DialContext: resolver.DialContext,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	client.Get("http://example.com")
}
```

### Force IPv4 (TCP4)

If you need to establish a raw TCP connection using only IPv4 (e.g., for custom protocols or raw socket usage), you can call `DialContext` with "tcp4".

```go
resolver := dnscache.New(dnscache.Config{})

// Establish a TCP connection to google.com:80 using IPv4 only
// This will perform a DNS lookup for A records only, avoiding AAAA records.
conn, err := resolver.DialContext(context.Background(), "tcp4", "google.com:80")
if err != nil {
    // Handle error
}
defer conn.Close()

// Use conn as a standard net.Conn...
```

### Direct Lookup

You can also use the resolver directly to lookup IPs:

```go
ips, err := resolver.LookupHost(context.Background(), "example.com")
// ips: ["93.184.216.34", ...]
```

### Custom DNS Server

You can specify a custom DNS server (e.g., Google Public DNS, Cloudflare DNS) instead of using the system default:

```go
resolver := dnscache.New(dnscache.Config{
    DNSServer: "8.8.8.8:53", // Google Public DNS
})

// Or use Cloudflare DNS
resolver := dnscache.New(dnscache.Config{
    DNSServer: "1.1.1.1:53",
})
```

### Custom Upstream Resolver (DoH, DoT, etc.)

For advanced use cases like DNS over HTTPS (DoH) or DNS over TLS (DoT), you can provide your own upstream resolver implementation:

```go
// Implement the DNSResolver interface
type DNSResolver interface {
    LookupHost(ctx context.Context, host string) (addrs []string, err error)
    LookupAddr(ctx context.Context, addr string) (names []string, err error)
    LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// Example: Using a DoH library (pseudo-code)
type DoHResolver struct {
    client *doh.Client
}

func (r *DoHResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
    // Implement DoH lookup logic here
    return r.client.Resolve(ctx, host)
}

// ... implement other methods ...

// Use the custom resolver
resolver := dnscache.New(dnscache.Config{
    Upstream: &DoHResolver{client: doh.NewClient("https://dns.google/dns-query")},
})
```

> **Note**: When both `Upstream` and `DNSServer` are specified, `Upstream` takes priority.

## IP Change Notifications

You can register a callback to be notified whenever the resolved IP addresses for a host change. This is particularly useful for updating downstream systems (like load balancers) in real-time.

```go
config := dnscache.Config{
    OnChange: func(host string, ips []string) {
        fmt.Printf("IPs changed for %s: %v\n", host, ips)
    },
}
```

## Observability & Metrics

### Cache Statistics

You can access runtime statistics (Hits/Misses) at any time. This is useful for exposing metrics to systems like Prometheus.

```go
stats := resolver.Stats()
fmt.Printf("Cache Hits: %d, Misses: %d\n", stats.CacheHits, stats.CacheMisses)
```

### Cache Miss Hook

You can register a callback to be notified whenever a cache miss occurs (and a real DNS query is issued).

```go
config := dnscache.Config{
    OnCacheMiss: func(host string) {
        // Increment your metric counter here
        fmt.Printf("Cache miss for: %s\n", host)
    },
}
```

### Distributed Tracing

The library fully supports `net/http/httptrace`. If your application uses a tracing library (like OpenTelemetry or Datadog) that injects `httptrace` into the context, `dnscache-go` will automatically report `DNSStart` and `DNSDone` events, ensuring your traces are complete.

## Configuration

You can fully customize the resolver behavior:

```go
config := dnscache.Config{
    // Enable logging or metrics on cache miss
    OnCacheMiss: func(host string) {
        fmt.Printf("Cache miss for: %s\n", host)
    },

    // Customize cache expiration (default: 1 minute)
    CacheTTL: 5 * time.Minute,

    // Customize expiration for failed lookups (Negative Caching).
    // Default is 1 second to prevent immediate retries (thundering herd)
    // while ensuring quick recovery.
    CacheFailTTL: 1 * time.Second,

    // Enable background refresh for "hot" domains.
    // When enabled, entries are refreshed at 50% TTL to ensure zero-latency cache hits.
    // Default is false. If enabled, remember to call resolver.Stop() when done.
    EnableAutoRefresh: true,

    // OnChange is executed when the resolved IPs for a host change.
    // It receives the host name and the new list of IP addresses.
    // This is useful for load balancer updates or service discovery triggers.
    OnChange: func(host string, ips []string) {
        fmt.Printf("IPs changed for %s: %v\n", host, ips)
    },

    // Automatically cleanup unused entries in the background
    // If set to 0 (default), no background cleanup runs.
    CleanupInterval: 10 * time.Minute,

    // Return stale cache data if upstream DNS fails.
    // If true, we will return expired IP records instead of the recent error
    // (until CacheFailTTL expires).
    PersistOnFailure: true,

    // If true, the cache acts as a transparent pass-through proxy.
    // It will directly call the underlying dialer, completely bypassing the cache.
    // Useful for testing or runtime toggles.
    Disabled: false,

    // Custom DNS server address (e.g., "8.8.8.8:53", "1.1.1.1:53").
    // If empty, uses the system default resolver.
    // This option is ignored if Upstream is set.
    DNSServer: "8.8.8.8:53",

    // Custom upstream resolver implementation.
    // Use this for DoH, DoT, or any custom DNS resolution logic.
    // If nil, uses the system default resolver (or DNSServer if specified).
    Upstream: myCustomResolver,
}

resolver := dnscache.New(config)

// If CleanupInterval or EnableAutoRefresh is set, remember to stop the background goroutine when done
// defer resolver.Stop()
```

## License

MIT License. See [LICENSE](LICENSE) file for details.

## Credits & Acknowledgments

This project is inspired by [rs/dnscache](https://github.com/rs/dnscache). 

It aims to build upon the original concept by adding production-oriented features like async refreshing, connection failover, and strict context handling.
