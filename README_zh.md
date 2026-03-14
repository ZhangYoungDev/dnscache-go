# dnscache-go

[English](README.md) | [中文文档](README_zh.md)

一个生产级的 Go DNS 缓存库，与 `net/http` 及标准库网络接口无缝集成。

旨在消除 DNS 延迟尖峰，并提供开箱即用的健壮连接处理能力。

## 为什么选择 dnscache-go？

本项目受 `rs/dnscache` 等现有方案的启发，但针对生产环境引入了多项关键工程改进：

- **智能异步刷新（可选）**：不同于传统缓存过期后阻塞等待，本库可以在 TTL 过半时自动在后台刷新"热点"域名。启用 `EnableAutoRefresh` 确保应用**始终命中缓存（0ms 延迟）**，在高流量期间无需等待 DNS 解析。
- **错误响应缓存**：自动缓存 DNS 查询失败，持续短暂时间（默认 1 秒）。这可以防止故障期间对上游 DNS 服务器的"缓存击穿"，同时确保服务恢复后快速重试。
- **标准 `DialContext` 带故障转移**：直接实现标准 `DialContext` 接口，可作为 `http.Transport` 的即插即用替代品。内置连接故障转移（简化版 Happy Eyeballs），当第一个 IP 失败时自动尝试下一个。
- **可配置 TTL**：支持显式配置缓存 TTL。条目按确定性过期，后台清理器（可选）确保移除未使用的条目，无需依赖手动刷新触发即可防止内存泄漏。
- **完整的 Context 支持**：完全尊重 Go 的 `context` 传播，包括取消和截止时间，不会破坏调用链。

## 功能特性

- **即插即用**：实现 `DialContext`，可轻松集成到 `http.Transport`。
- **缓存击穿保护**：使用 `singleflight` 合并并发 DNS 查询。
- **零延迟**：异步自动刷新（启用时）保持活跃域名的缓存预热。
- **高可用**：连接故障转移确保对单个 IP 故障的健壮性。
- **可观测性**：内置指标（`CacheHits`/`CacheMisses`）、`OnCacheMiss` 钩子和 `httptrace` 支持。
- **动态更新**：`OnChange` 钩子允许实时响应 IP 变化（例如用于服务发现）。
- **链路追踪支持**：尊重 `httptrace.ClientTrace` 上下文，用于详细的性能分析。
- **自定义 DNS 服务器**：支持自定义 DNS 服务器地址（如 `8.8.8.8:53`、`1.1.1.1:53`），可绕过系统 DNS。
- **可插拔上游**：允许自定义 `DNSResolver` 实现，支持 DoH（DNS over HTTPS）、DoT（DNS over TLS）或任何其他 DNS 协议。
- **零配置**：开箱即用，默认配置合理。

## 安装

```bash
go get github.com/ZhangYoungDev/dnscache-go
```

## 使用方法

### 配合 `http.Client` 使用（默认）

这是最常见的使用场景。通过替换 `http.Transport` 中的 `DialContext`，客户端发出的所有请求都会自动使用 DNS 缓存。

```go
package main

import (
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

### 配合 `http.Client` 使用（仅 IPv4）

使用 `NewOnlyV4` 创建仅查询 A 记录（IPv4）的解析器。客户端发出的所有请求都只会使用 IPv4 地址。

```go
package main

import (
	"net/http"
	"time"

	"github.com/ZhangYoungDev/dnscache-go"
)

func main() {
	resolver := dnscache.NewOnlyV4(dnscache.Config{})

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

> **提示**：类似地，使用 `NewOnlyV6` 可以实现仅 IPv6 解析。

### 原始 TCP 连接

你可以使用 `DialContext` 建立带有 DNS 缓存和自动故障转移的原始 TCP 连接。

```go
resolver := dnscache.New(dnscache.Config{})

conn, err := resolver.DialContext(context.Background(), "tcp", "google.com:80")
if err != nil {
    // 处理错误
}
defer conn.Close()

// 将 conn 作为标准 net.Conn 使用...
```

如需强制仅使用 IPv4，请使用 `NewOnlyV4`。这将确保 DNS 查询只请求 A 记录，缓存中不会包含多余的 AAAA 条目。

```go
resolver := dnscache.NewOnlyV4(dnscache.Config{})

conn, err := resolver.DialContext(context.Background(), "tcp", "google.com:80")
if err != nil {
    // 处理错误
}
defer conn.Close()
```

### 直接查询

你也可以直接使用 resolver 查询 IP：

```go
ips, err := resolver.LookupHost(context.Background(), "example.com")
// ips: ["93.184.216.34", ...]
```

### 自定义 DNS 服务器

你可以指定自定义 DNS 服务器（如 Google Public DNS、Cloudflare DNS）替代系统默认：

```go
resolver := dnscache.New(dnscache.Config{
    DNSServer: "8.8.8.8:53", // Google Public DNS
})

// 或使用 Cloudflare DNS
resolver := dnscache.New(dnscache.Config{
    DNSServer: "1.1.1.1:53",
})
```

### 自定义上游解析器（DoH、DoT 等）

对于 DNS over HTTPS (DoH) 或 DNS over TLS (DoT) 等高级用例，你可以提供自己的上游解析器实现：

```go
// 实现 DNSResolver 接口
type DNSResolver interface {
    LookupHost(ctx context.Context, host string) (addrs []string, err error)
    LookupAddr(ctx context.Context, addr string) (names []string, err error)
    LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// 示例：使用 DoH 库（伪代码）
type DoHResolver struct {
    client *doh.Client
}

func (r *DoHResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
    // 在此实现 DoH 查询逻辑
    return r.client.Resolve(ctx, host)
}

// ... 实现其他方法 ...

// 使用自定义解析器
resolver := dnscache.New(dnscache.Config{
    Upstream: &DoHResolver{client: doh.NewClient("https://dns.google/dns-query")},
})
```

> **注意**：当同时指定 `Upstream` 和 `DNSServer` 时，`Upstream` 优先级更高。

## IP 变更通知

你可以注册回调函数，在主机解析的 IP 地址发生变化时收到通知。这对于实时更新下游系统（如负载均衡器）特别有用。

```go
config := dnscache.Config{
    OnChange: func(host string, ips []string) {
        fmt.Printf("主机 %s 的 IP 已变更: %v\n", host, ips)
    },
}
```

## 可观测性与指标

### 缓存统计

你可以随时访问运行时统计信息（命中/未命中）。这对于将指标暴露给 Prometheus 等系统很有用。

```go
stats := resolver.Stats()
fmt.Printf("缓存命中: %d, 未命中: %d\n", stats.CacheHits, stats.CacheMisses)
```

### 缓存未命中钩子

你可以注册回调函数，在缓存未命中时收到通知（此时会发起真实的 DNS 查询）。

```go
config := dnscache.Config{
    OnCacheMiss: func(host string) {
        // 在此增加你的指标计数器
        fmt.Printf("缓存未命中: %s\n", host)
    },
}
```

### 分布式追踪

本库完全支持 `net/http/httptrace`。如果你的应用使用了将 `httptrace` 注入上下文的追踪库（如 OpenTelemetry 或 Datadog），`dnscache-go` 将自动报告 `DNSStart` 和 `DNSDone` 事件，确保你的追踪链路完整。

## 配置

你可以完全自定义解析器行为：

```go
config := dnscache.Config{
    // 在缓存未命中时启用日志或指标
    OnCacheMiss: func(host string) {
        fmt.Printf("缓存未命中: %s\n", host)
    },

    // 自定义缓存过期时间（默认: 1 分钟）
    CacheTTL: 5 * time.Minute,

    // 自定义失败查询的过期时间（错误响应缓存）
    // 默认 1 秒，防止立即重试（惊群效应）
    // 同时确保快速恢复
    CacheFailTTL: 1 * time.Second,

    // 为"热点"域名启用后台刷新
    // 启用后，条目在 50% TTL 时刷新，确保零延迟缓存命中
    // 默认为 false。如果启用，记得在完成后调用 resolver.Stop()
    EnableAutoRefresh: true,

    // 当主机解析的 IP 变化时执行
    // 接收主机名和新的 IP 地址列表
    // 适用于负载均衡器更新或服务发现触发
    OnChange: func(host string, ips []string) {
        fmt.Printf("主机 %s 的 IP 已变更: %v\n", host, ips)
    },

    // 在后台自动清理未使用的条目
    // 如果设为 0（默认），则不运行后台清理
    CleanupInterval: 10 * time.Minute,

    // 上游 DNS 失败时返回过期的缓存数据
    // 如果为 true，将返回过期的 IP 记录而不是最近的错误
    // （直到 CacheFailTTL 过期）
    PersistOnFailure: true,

    // 如果为 true，缓存作为透明直通代理
    // 将直接调用底层 dialer，完全绕过缓存
    // 适用于测试或运行时开关
    Disabled: false,

    // 自定义 DNS 服务器地址（如 "8.8.8.8:53"、"1.1.1.1:53"）
    // 如果为空，使用系统默认解析器
    // 如果设置了 Upstream，此选项将被忽略
    DNSServer: "8.8.8.8:53",

    // 自定义上游解析器实现
    // 用于 DoH、DoT 或任何自定义 DNS 解析逻辑
    // 如果为 nil，使用系统默认解析器（或 DNSServer，如果已指定）
    Upstream: myCustomResolver,
}

resolver := dnscache.New(config)

// 如果设置了 CleanupInterval 或 EnableAutoRefresh，记得在完成后停止后台协程
// defer resolver.Stop()
```

## 许可证

MIT 许可证。详见 [LICENSE](LICENSE) 文件。

## 致谢

本项目受 [rs/dnscache](https://github.com/rs/dnscache) 启发。

旨在基于原有概念进行扩展，添加面向生产环境的特性，如异步刷新、连接故障转移和严格的 context 处理。