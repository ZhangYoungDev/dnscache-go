package dnscache

import (
	"context"
	"math/rand"
	"net"
	"sync/atomic"
)

// DialContext connects to the address on the named network using the provided context.
func (r *Resolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if r.config.Disabled {
		return r.dialer.DialContext(ctx, network, address)
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	ips, err := r.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}

	if len(ips) == 0 {
		return nil, &net.OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: net.UnknownNetworkError("no addresses found")}
	}

	// Apply dial strategy
	orderedIPs := r.applyDialStrategy(ips)

	var lastErr error
	for _, ip := range orderedIPs {
		targetAddr := net.JoinHostPort(ip, port)
		conn, err := r.dialer.DialContext(ctx, network, targetAddr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}

	return nil, lastErr
}

// applyDialStrategy reorders the IPs according to the configured strategy.
func (r *Resolver) applyDialStrategy(ips []string) []string {
	if len(ips) <= 1 {
		return ips
	}

	switch r.config.DialStrategy {
	case DialStrategySequential:
		// Return as-is (DNS response order)
		return ips

	case DialStrategyRoundRobin:
		// Rotate starting index for each call
		idx := atomic.AddUint32(&r.rrIndex, 1) - 1
		startIdx := int(idx) % len(ips)
		result := make([]string, len(ips))
		for i := 0; i < len(ips); i++ {
			result[i] = ips[(startIdx+i)%len(ips)]
		}
		return result

	case DialStrategyRandom:
		fallthrough
	default:
		// Shuffle randomly (default)
		result := make([]string, len(ips))
		copy(result, ips)
		rand.Shuffle(len(result), func(i, j int) {
			result[i], result[j] = result[j], result[i]
		})
		return result
	}
}
