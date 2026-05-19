package main

import (
        "fmt"
        "math"
        "net/http"
        "net/url"
        "strings"
        "sync"
        "time"
)

// ──────────────────────── Error Categorization (B3) ───────────────────────

type ErrorCategory int

const (
        ErrCatUnknown ErrorCategory = iota
        ErrCatNetworkTimeout
        ErrCatAPIError       // API returned 5xx or malformed response
        ErrCatProxyFail      // Proxy connection refused, auth fail, bad gateway
        ErrCatStoreFail      // Shopify site error (checkout unavailable, maintenance)
        ErrCatRateLimited    // Too many requests
        ErrCatDeclined       // True card decline (no retry)
)

func categorizeError(err error, statusCode string) ErrorCategory {
        if err == nil {
                return ErrCatUnknown
        }
        msg := strings.ToLower(err.Error())
        sc := strings.ToLower(statusCode)

        // True declines — never retry
        if strings.Contains(msg, "card_declined") ||
                strings.Contains(msg, "captcha_required") ||
                strings.Contains(msg, "fraud_suspected") ||
                strings.Contains(msg, "insufficient_funds") ||
                strings.Contains(sc, "card_declined") {
                return ErrCatDeclined
        }

        // Proxy failures
        if strings.Contains(msg, "proxy") ||
                strings.Contains(msg, "connection refused") ||
                strings.Contains(msg, "connection reset") ||
                strings.Contains(msg, "proxy authentication") ||
                strings.Contains(msg, "tls handshake") ||
                strings.Contains(msg, "dial tcp") ||
                strings.Contains(msg, "socks") ||
                strings.Contains(msg, "bad gateway") ||
                strings.Contains(msg, "gateway timeout") {
                return ErrCatProxyFail
        }

        // Network / timeout
        if strings.Contains(msg, "timeout") ||
                strings.Contains(msg, "context deadline exceeded") ||
                strings.Contains(msg, "no such host") ||
                strings.Contains(msg, "temporary failure in name resolution") ||
                strings.Contains(msg, "i/o timeout") {
                return ErrCatNetworkTimeout
        }

        // Rate limited
        if strings.Contains(msg, "rate limit") ||
                strings.Contains(msg, "too many requests") ||
                strings.Contains(msg, "429") ||
                strings.Contains(sc, "rate_limit") {
                return ErrCatRateLimited
        }

        // Store failures
        if strings.Contains(msg, "checkout") ||
                strings.Contains(msg, "store error") ||
                strings.Contains(msg, "site error") ||
                strings.Contains(msg, "shopify") ||
                strings.Contains(sc, "store") ||
                strings.Contains(sc, "maintenance") {
                return ErrCatStoreFail
        }

        // API errors (5xx, empty response, parse errors)
        if strings.Contains(msg, "api") ||
                strings.Contains(msg, "status 5") ||
                strings.Contains(msg, "empty response") ||
                strings.Contains(msg, "parsing") ||
                strings.Contains(msg, "json") ||
                strings.Contains(msg, "unmarshal") {
                return ErrCatAPIError
        }

        return ErrCatUnknown
}

// IsRetryable returns true if this error category warrants a retry with a different store/proxy.
func (ec ErrorCategory) IsRetryable() bool {
        switch ec {
        case ErrCatNetworkTimeout, ErrCatAPIError, ErrCatProxyFail, ErrCatStoreFail, ErrCatRateLimited, ErrCatUnknown:
                return true
        case ErrCatDeclined:
                return false
        }
        return false
}

// BackoffDuration returns the recommended wait before retrying.
func (ec ErrorCategory) BackoffDuration(attempt int) time.Duration {
        base := time.Second
        switch ec {
        case ErrCatRateLimited:
                base = 3 * time.Second
        case ErrCatProxyFail:
                base = 500 * time.Millisecond
        case ErrCatNetworkTimeout:
                base = 2 * time.Second
        case ErrCatAPIError:
                base = 1 * time.Second
        case ErrCatStoreFail:
                base = 1 * time.Second
        }
        // Exponential backoff with jitter
        jitter := time.Duration(math.Pow(2, float64(attempt))) * base
        if jitter > 30*time.Second {
                jitter = 30 * time.Second
        }
        return jitter
}

func (ec ErrorCategory) String() string {
        switch ec {
        case ErrCatNetworkTimeout:
                return "NETWORK_TIMEOUT"
        case ErrCatAPIError:
                return "API_ERROR"
        case ErrCatProxyFail:
                return "PROXY_FAIL"
        case ErrCatStoreFail:
                return "STORE_FAIL"
        case ErrCatRateLimited:
                return "RATE_LIMITED"
        case ErrCatDeclined:
                return "DECLINED"
        }
        return "UNKNOWN"
}

// ──────────────────────── Proxy Health Manager (B4) ───────────────────────

var (
        proxyHealthMu sync.RWMutex
        proxyHealthDB map[string]*ProxyHealth // in-memory cache
)

func initProxyHealth() {
        proxyHealthMu.Lock()
        defer proxyHealthMu.Unlock()
        proxyHealthDB = make(map[string]*ProxyHealth)

        if !isMongo() {
                return
        }
        db, err := mongoLoadAllProxyHealth()
        if err != nil {
                fmt.Printf("[PROXY] failed to load health from MongoDB: %v\n", err)
                return
        }
        proxyHealthDB = db
        fmt.Printf("[PROXY] loaded %d proxy health records\n", len(db))
}

func getProxyHealth(proxyURL string) *ProxyHealth {
        proxyHealthMu.RLock()
        ph := proxyHealthDB[proxyURL]
        proxyHealthMu.RUnlock()
        if ph == nil {
                return &ProxyHealth{ProxyURL: proxyURL, Healthy: true}
        }
        return ph
}

// recordProxyResult updates health after a card check result.
func recordProxyResult(proxyURL string, success bool, latencyMs float64) {
        proxyHealthMu.Lock()
        ph := proxyHealthDB[proxyURL]
        if ph == nil {
                ph = &ProxyHealth{ProxyURL: proxyURL, Healthy: true}
                proxyHealthDB[proxyURL] = ph
        }

        if success {
                ph.Successes++
                ph.Consecutive = 0
                ph.Healthy = true
        } else {
                ph.Failures++
                ph.Consecutive++
                // Mark unhealthy after 3 consecutive failures
                if ph.Consecutive >= 3 {
                        ph.Healthy = false
                }
        }
        ph.LastUsed = time.Now()
        if latencyMs > 0 {
                // Running average
                total := ph.Successes + ph.Failures
                if total > 1 {
                        ph.AvgLatency = (ph.AvgLatency*float64(total-1) + latencyMs) / float64(total)
                } else {
                        ph.AvgLatency = latencyMs
                }
        }
        proxyHealthMu.Unlock()

        // Persist to MongoDB async
        if isMongo() {
                go func() {
                        _ = mongoRecordProxyHealth(ph)
                }()
        }
}

// healthyProxies filters a proxy list to healthy ones, sorted by latency.
func healthyProxies(proxies []string) []string {
        proxyHealthMu.RLock()
        defer proxyHealthMu.RUnlock()

        var healthy []struct {
                url  string
                lat  float64
                succ int64
        }
        for _, p := range proxies {
                ph := proxyHealthDB[p]
                if ph == nil || ph.Healthy {
                        healthy = append(healthy, struct {
                                url  string
                                lat  float64
                                succ int64
                        }{url: p, lat: ph.AvgLatency, succ: ph.Successes})
                }
        }
        if len(healthy) == 0 {
                // All marked unhealthy — return original list and let them retry
                return proxies
        }

        // Sort by latency ascending (lower = better), break ties by success count
        for i := range healthy {
                for j := i + 1; j < len(healthy); j++ {
                        if healthy[i].lat > healthy[j].lat ||
                                (healthy[i].lat == healthy[j].lat && healthy[i].succ < healthy[j].succ) {
                                healthy[i], healthy[j] = healthy[j], healthy[i]
                        }
                }
        }
        out := make([]string, len(healthy))
        for i, h := range healthy {
                out[i] = h.url
        }
        return out
}

// markProxyCheck records a pre-check (alive/dead) result.
func markProxyCheck(proxyURL string, ok bool, latency time.Duration) {
        ms := float64(latency.Milliseconds())
        proxyHealthMu.Lock()
        ph := proxyHealthDB[proxyURL]
        if ph == nil {
                ph = &ProxyHealth{ProxyURL: proxyURL, Healthy: true}
                proxyHealthDB[proxyURL] = ph
        }
        ph.LastCheck = time.Now()
        if ok {
                ph.Consecutive = 0
                ph.Healthy = true
        } else {
                ph.Consecutive++
                if ph.Consecutive >= 3 {
                        ph.Healthy = false
                }
        }
        if ms > 0 {
                ph.AvgLatency = (ph.AvgLatency + ms) / 2
        }
        proxyHealthMu.Unlock()

        if isMongo() {
                go func() {
                        _ = mongoRecordProxyHealth(ph)
                }()
        }
}

// testProxyWithLatency returns error and measured latency.
func testProxyWithLatency(proxyURL string) (error, time.Duration) {
        start := time.Now()
        err := testProxy(proxyURL)
        return err, time.Since(start)
}

// aliveProxiesWithHealth runs parallel health checks and records results.
func aliveProxiesWithHealth(proxies []string) []string {
        var aliveMu sync.Mutex
        var wg sync.WaitGroup
        alive := make([]string, 0, len(proxies))

        for _, p := range proxies {
                wg.Add(1)
                go func(proxy string) {
                        defer wg.Done()
                        err, latency := testProxyWithLatency(proxy)
                        markProxyCheck(proxy, err == nil, latency)
                        if err == nil {
                                aliveMu.Lock()
                                alive = append(alive, proxy)
                                aliveMu.Unlock()
                        }
                }(p)
        }
        wg.Wait()

        // Return sorted by health (fastest first)
        if len(alive) > 0 {
                return healthyProxies(alive)
        }
        return alive
}

// newHTTPClientWithProxy creates an http.Client with the given proxy and timeout.
func newHTTPClientWithProxy(proxyURL string, timeout time.Duration) *http.Client {
        if proxyURL == "" {
                return &http.Client{Timeout: timeout}
        }
        pu, err := url.Parse(proxyURL)
        if err != nil {
                return &http.Client{Timeout: timeout}
        }
        return &http.Client{
                Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
                Timeout:   timeout,
        }
}
