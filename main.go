package main

import (
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- CONFIGURATION ---
const (
	// ULTRA-FAST: Page 1, only 5 items — absolute minimum payload for new arrivals
	API_URL_ULTRA_FAST = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=5&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="

	// FAST: Page 1, 20 items — broader check
	API_URL_FAST = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=20&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="

	// FULL-SCAN: Page 1 with 100 items
	API_URL_PAGE_1 = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=100&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="

	// PAGING: Pages 2+
	API_URL_PAGING_TEMPLATE = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsPaging?PageNo=%d&PageSize=20&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&sorting=&MasterBrand=113&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="

	PAGES_TO_SCAN          = 6
	TELEGRAM_BOT_TOKEN     = "8336369415:AAE7idSEyOpMIUlYhL4z9yze0C4_6rdbzE4"
	TELEGRAM_CHAT_ID       = "-4985438208"
	ADMIN_CHAT_ID          = "837428747"
	SEEN_ITEMS_FILE        = "seen_hotwheels_go.txt"
	CART_CONFIG_FILE       = "cart_config.json"
	RESTOCK_WATCHLIST_FILE = "restock_watchlist.json"

	// --- CART API ---
	CART_API_URL  = "https://www.firstcry.com/svcs/CommonService.svc/SaveCartDetail"
	CART_SAVE_URL = "https://www.firstcry.com/capinet/pdp/SaveProductCart"

	// --- SPEED TUNING (v3 — ULTRA AGGRESSIVE) ---
	ULTRA_FAST_INTERVAL   = 2 * time.Second  // Tiny 5-item check every 2 seconds (was 3s)
	FAST_POLL_INTERVAL    = 8 * time.Second  // 20-item check every 8 seconds (was 10s)
	FULL_SCAN_INTERVAL    = 60 * time.Second // Full 6-page scan every 60 seconds (was 90s)
	RESTOCK_POLL_INTERVAL = 3 * time.Second  // Restock watchlist poll every 3 seconds (was 5s)
	HTTP_TIMEOUT          = 3 * time.Second  // Faster timeout (was 4s)
	TELEGRAM_TIMEOUT      = 20 * time.Second // Faster Telegram timeout
	CART_TIMEOUT          = 10 * time.Second // Cart timeout (was 15s) — must succeed but faster fail
	CART_MAX_RETRIES      = 4                // Retry up to 4 times (was 3)
	RESTOCK_MAX_CONCURRENT = 4               // Max concurrent restock checks
)

// --- PERSISTENT HTTP CLIENTS (connection reuse + TCP keep-alive) ---
var (
	// Shared dialer with aggressive keep-alive for persistent connections
	sharedDialer = &net.Dialer{
		Timeout:   2 * time.Second,
		KeepAlive: 30 * time.Second, // TCP-level keep-alive probes
	}

	apiClient = &http.Client{
		Timeout: HTTP_TIMEOUT,
		Transport: &http.Transport{
			DialContext:           sharedDialer.DialContext,
			MaxIdleConns:          30,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       180 * time.Second,
			TLSHandshakeTimeout:   2 * time.Second,
			ResponseHeaderTimeout: 3 * time.Second,
			DisableCompression:    false,
			ForceAttemptHTTP2:     true,
			WriteBufferSize:       8192,
			ReadBufferSize:        16384,
		},
	}

	telegramClient = &http.Client{
		Timeout: TELEGRAM_TIMEOUT,
		Transport: &http.Transport{
			DialContext:           sharedDialer.DialContext,
			MaxIdleConns:          10,
			MaxIdleConnsPerHost:   5,
			IdleConnTimeout:       180 * time.Second,
			TLSHandshakeTimeout:   2 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
			ForceAttemptHTTP2:     true,
			WriteBufferSize:       4096,
			ReadBufferSize:        4096,
		},
	}

	// Dedicated cart client — fast + reliable, this is the most important request
	cartClient = &http.Client{
		Timeout: CART_TIMEOUT,
		Transport: &http.Transport{
			DialContext:           sharedDialer.DialContext,
			MaxIdleConns:          15,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       180 * time.Second,
			TLSHandshakeTimeout:   2 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			ForceAttemptHTTP2:     true,
			WriteBufferSize:       4096,
			ReadBufferSize:        4096,
		},
	}
)

// --- SHARED STATE ---
var (
	mutex             sync.Mutex
	ultraFastInterval = ULTRA_FAST_INTERVAL
	fullScanInterval  = FULL_SCAN_INTERVAL
	isPaused          = false
	heartbeatMuted    = false
	seenItems         = make(map[string]bool)
	checkHistory      []CheckResult

	// Async Telegram pipeline
	telegramQueue = make(chan telegramMsg, 100)

	// ETag cache — skip re-downloading unchanged responses
	etagCache   = make(map[string]string) // URL -> ETag
	etagCacheMu sync.RWMutex

	// Content hash — detect changes without full product comparison
	lastContentHash   string
	lastContentHashMu sync.Mutex

	// Stats
	totalChecks   int64
	skippedChecks int64 // Skipped due to no change detected
	totalNewItems int64
	totalCarted   int64
	startTime     time.Time

	// Cart config
	cartConfig   CartConfig
	cartConfigMu sync.RWMutex

	// Restock watchlist
	restockWatchlist   RestockWatchlist
	restockWatchlistMu sync.RWMutex
)

// --- CART CONFIG ---
type CartConfig struct {
	Enabled bool   `json:"enabled"`
	Cookies string `json:"cookies"`
	Ftk     string `json:"ftk"`
}

// --- RESTOCK WATCHLIST ---
type WatchProduct struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	LastStock string `json:"last_stock"` // "0" = out of stock
}

type RestockWatchlist struct {
	Enabled          bool           `json:"enabled"`
	PollIntervalSecs int            `json:"poll_interval_seconds"`
	Products         []WatchProduct `json:"products"`
}

func loadCartConfig() {
	data, err := os.ReadFile(CART_CONFIG_FILE)
	if err != nil {
		log.Printf("⚠️ No cart config found (%s) — auto add-to-cart disabled", CART_CONFIG_FILE)
		return
	}
	cartConfigMu.Lock()
	defer cartConfigMu.Unlock()
	if err := json.Unmarshal(data, &cartConfig); err != nil {
		log.Printf("❌ Cart config parse error: %v", err)
		return
	}
	if cartConfig.Enabled {
		log.Println("🛒 Auto add-to-cart: ENABLED")
	} else {
		log.Println("🛒 Auto add-to-cart: DISABLED")
	}
}

func saveCartConfig() {
	cartConfigMu.RLock()
	data, _ := json.MarshalIndent(cartConfig, "", "    ")
	cartConfigMu.RUnlock()
	os.WriteFile(CART_CONFIG_FILE, data, 0644)
}

func loadRestockWatchlist() {
	data, err := os.ReadFile(RESTOCK_WATCHLIST_FILE)
	if err != nil {
		log.Printf("⚠️ No restock watchlist found (%s) — restock monitor disabled", RESTOCK_WATCHLIST_FILE)
		return
	}
	restockWatchlistMu.Lock()
	defer restockWatchlistMu.Unlock()
	if err := json.Unmarshal(data, &restockWatchlist); err != nil {
		log.Printf("❌ Restock watchlist parse error: %v", err)
		return
	}
	if restockWatchlist.PollIntervalSecs < 1 {
		restockWatchlist.PollIntervalSecs = 5
	}
	if restockWatchlist.Enabled {
		log.Printf("👁️ Restock watchlist: ENABLED (%d products, every %ds)", len(restockWatchlist.Products), restockWatchlist.PollIntervalSecs)
	} else {
		log.Println("👁️ Restock watchlist: DISABLED")
	}
}

func saveRestockWatchlist() {
	restockWatchlistMu.RLock()
	data, _ := json.MarshalIndent(restockWatchlist, "", "    ")
	restockWatchlistMu.RUnlock()
	os.WriteFile(RESTOCK_WATCHLIST_FILE, data, 0644)
}

// Extract product ID from a FirstCry URL like:
// https://www.firstcry.com/hot-wheels/some-name/15837901/product-detail
func extractProductIDFromURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	// Try to find a numeric segment that looks like a product ID
	parts := strings.Split(rawURL, "/")
	for _, part := range parts {
		if len(part) >= 5 && len(part) <= 15 {
			if _, err := strconv.Atoi(part); err == nil {
				return part
			}
		}
	}
	return ""
}

// Extract product name from FirstCry URL slug
func extractNameFromURL(rawURL string) string {
	parts := strings.Split(rawURL, "/")
	for i, part := range parts {
		// The slug is typically right before the product ID
		if i+1 < len(parts) {
			if _, err := strconv.Atoi(parts[i+1]); err == nil && len(parts[i+1]) >= 5 {
				return strings.ReplaceAll(part, "-", " ")
			}
		}
	}
	return "Unknown Product"
}

// --- ADD TO CART (BULLETPROOF — retries + validation + fallback) ---
func addToCart(p Product) {
	cartConfigMu.RLock()
	if !cartConfig.Enabled || cartConfig.Ftk == "" || cartConfig.Cookies == "" {
		cartConfigMu.RUnlock()
		log.Printf("🛒 Cart skip (disabled/no config): %s", p.ProductName)
		return
	}
	ftk := cartConfig.Ftk
	cookies := cartConfig.Cookies
	cartConfigMu.RUnlock()

	fullURL := constructFullURL(p)
	overallStart := time.Now()

	// Pre-build the payload string to avoid formatting overhead inside the loop (Optimization Item 2)
	payloadStr := fmt.Sprintf(`{"ftk":%q,"viewid":"","productid":%q,"quantity":"1","offertype":"NO","offerid":"","action":"add","gcoffer":""}`, ftk, p.ProductID)

	// --- ATTEMPT PRIMARY API (SaveCartDetail) WITH RETRIES ---
	var lastErr error
	var lastStatus int
	var lastBody string
	success := false

	for attempt := 1; attempt <= CART_MAX_RETRIES; attempt++ {
		if attempt > 1 {
			// Reduced backoff: 200ms, 500ms, 1s (was 500ms, 1s, 2s)
			backoff := time.Duration(1<<(attempt-2)) * 200 * time.Millisecond
			if backoff > 1*time.Second {
				backoff = 1 * time.Second
			}
			log.Printf("🔄 Cart retry %d/%d in %v for: %s", attempt, CART_MAX_RETRIES, backoff, p.ProductName)
			time.Sleep(backoff)
		}

		// Use the pre-built payload string
		req, err := http.NewRequest("POST", CART_API_URL, strings.NewReader(payloadStr))
		if err != nil {
			lastErr = fmt.Errorf("build request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", "https://www.firstcry.com")
		req.Header.Set("Referer", "https://www.firstcry.com/")
		req.Header.Set("Cookie", cookies)

		resp, err := cartClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("network: %v", err)
			log.Printf("❌ Cart attempt %d failed (network): %v", attempt, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastStatus = resp.StatusCode
		lastBody = string(body)

		if resp.StatusCode == 200 {
			// Validate response body — check it's not an error page or auth failure
			bodyStr := strings.ToLower(lastBody)
			if strings.Contains(bodyStr, "login") || strings.Contains(bodyStr, "session") || strings.Contains(bodyStr, "unauthorized") {
				lastErr = fmt.Errorf("auth expired (response contains login/session)")
				log.Printf("⚠️ Cart attempt %d: HTTP 200 but auth seems expired: %.100s", attempt, lastBody)
				// Auth expired — no point retrying with same cookies
				sendTelegramUrgent(ADMIN_CHAT_ID, fmt.Sprintf(
					"🔑❌ <b>Cart cookies/FTK EXPIRED!</b>\n\nCannot add: %s\nUse /updatecookies and /updateftk to refresh.\n\n<a href='%s'>🛒 Add Manually →</a>",
					p.ProductName, fullURL,
				))
				break
			}
			// SUCCESS!
			success = true
			break
		} else if resp.StatusCode >= 500 {
			// Server error — retry
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			log.Printf("⚠️ Cart attempt %d: server error %d, will retry...", attempt, resp.StatusCode)
			continue
		} else {
			// 4xx or other — likely won't help to retry
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, lastBody)
			log.Printf("❌ Cart attempt %d: HTTP %d — %.200s", attempt, resp.StatusCode, lastBody)
			break
		}
	}

	elapsed := time.Since(overallStart)

	if success {
		log.Printf("🛒✅ ADDED TO CART in %dms: %s (PID: %s)", elapsed.Milliseconds(), p.ProductName, p.ProductID)
		mutex.Lock()
		totalCarted++
		mutex.Unlock()
		sendTelegramUrgent(TELEGRAM_CHAT_ID, fmt.Sprintf(
			"🛒✅ <b>Auto-Added to Cart!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> ₹%s\n⚡ Added in %dms\n\n<a href='%s'>🔗 View Product</a>",
			p.ProductName, p.Price, elapsed.Milliseconds(), fullURL,
		))

		// Request 2: SaveProductCart (secondary confirmation) — also with retry
		go func() {
			for r := 1; r <= 2; r++ {
				payload2 := fmt.Sprintf(`{"objdd":{"ProductID":%q,"ViewID":"","ProductType":"product","cart":"cart","Discount":""},"ftk":%q}`, p.ProductID, ftk)
				req2, _ := http.NewRequest("POST", CART_SAVE_URL, bytes.NewBufferString(payload2))
				req2.Header.Set("Content-Type", "application/json; charset=utf-8")
				req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
				req2.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
				req2.Header.Set("X-Requested-With", "XMLHttpRequest")
				req2.Header.Set("Origin", "https://www.firstcry.com")
				req2.Header.Set("Referer", "https://www.firstcry.com/")
				req2.Header.Set("Cookie", cookies)
				resp2, err := cartClient.Do(req2)
				if err != nil {
					log.Printf("⚠️ Cart save (secondary) attempt %d failed: %v", r, err)
					time.Sleep(1 * time.Second)
					continue
				}
				resp2.Body.Close()
				log.Printf("🛒 Secondary cart save OK for: %s", p.ProductName)
				return
			}
		}()
	} else {
		// ALL RETRIES FAILED — send URGENT fallback with buy link
		log.Printf("❌❌ ALL %d CART ATTEMPTS FAILED for: %s (last error: %v)", CART_MAX_RETRIES, p.ProductName, lastErr)
		sendTelegramUrgent(TELEGRAM_CHAT_ID, fmt.Sprintf(
			"🚨🛒 <b>CART FAILED — ADD MANUALLY!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> ₹%s\n\n<b>Error:</b> %v (HTTP %d)\n\n👉 <a href='%s'>ADD TO CART MANUALLY →</a>",
			p.ProductName, p.Price, lastErr, lastStatus, fullURL,
		))
		sendTelegramUrgent(ADMIN_CHAT_ID, fmt.Sprintf(
			"❌ Cart failed after %d attempts: %s\nLast error: %v\nHTTP %d: %.300s",
			CART_MAX_RETRIES, p.ProductName, lastErr, lastStatus, lastBody,
		))
	}
}

// --- TELEGRAM ASYNC PIPELINE ---
type telegramMsg struct {
	ChatID   string
	Message  string
	Priority bool // High priority = new item alerts
}

func telegramSenderWorker() {
	for msg := range telegramQueue {
		doSendTelegram(msg.ChatID, msg.Message)
	}
}

func doSendTelegram(chatID, message string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TELEGRAM_BOT_TOKEN)
	payload := url.Values{}
	payload.Set("chat_id", chatID)
	payload.Set("text", message)
	payload.Set("parse_mode", "HTML")
	resp, err := telegramClient.PostForm(apiURL, payload)
	if err != nil {
		log.Printf("❌ Telegram send failed: %v", err)
		return
	}
	resp.Body.Close()
}

// Non-blocking Telegram send
func sendTelegramMessage(chatID, message string) {
	select {
	case telegramQueue <- telegramMsg{ChatID: chatID, Message: message}:
	default:
		log.Println("⚠️ Telegram queue full, dropping message")
	}
}

// High-priority send (for new item alerts — bypasses queue, sends directly in goroutine)
func sendTelegramUrgent(chatID, message string) {
	go doSendTelegram(chatID, message)
}

// --- STRUCTS ---
type TelegramUpdateResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}
type Update struct {
	UpdateID int     `json:"update_id"`
	Message  Message `json:"message"`
}
type Message struct {
	Text string `json:"text"`
	Chat Chat   `json:"chat"`
}
type Chat struct {
	ID int64 `json:"id"`
}

type OuterEnvelope struct {
	ProductResponse string `json:"ProductResponse"`
}
type InnerData struct {
	Products []Product `json:"Products"`
}
type Product struct {
	ProductID     string `json:"PId"`
	ProductInfoID string `json:"PInfId"`
	ProductName   string `json:"PNm"`
	Price         string `json:"discprice"`
	StockStatus   string `json:"CrntStock"`
}

type CheckResult struct {
	Timestamp     time.Time
	FoundProducts []Product
}

// --- HELPERS ---
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9 ]+`)
var spaceRegex = regexp.MustCompile(`\s+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphanumericRegex.ReplaceAllString(s, "")
	s = spaceRegex.ReplaceAllString(s, "-")
	return s
}

func constructFullURL(p Product) string {
	return fmt.Sprintf("https://www.firstcry.com/hot-wheels/%s/%s/product-detail", slugify(p.ProductName), p.ProductID)
}

func loadSeenItems() {
	data, err := os.ReadFile(SEEN_ITEMS_FILE)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	uniqueLines := []string{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			if !seenItems[line] {
				seenItems[line] = true
				uniqueLines = append(uniqueLines, line)
			}
		}
	}
	
	// Deduplicate on load to prevent the file from growing indefinitely (Optimization Item 10)
	if len(uniqueLines) < len(lines) {
		err = os.WriteFile(SEEN_ITEMS_FILE, []byte(strings.Join(uniqueLines, "\n")+"\n"), 0644)
		if err != nil {
			log.Printf("⚠️ Failed to write deduplicated seen items: %v", err)
		} else {
			log.Printf("🧹 Deduplicated seen items file (removed %d duplicates)", len(lines)-len(uniqueLines))
		}
	}
}

func saveNewItem(productInfoID string) {
	f, err := os.OpenFile(SEEN_ITEMS_FILE, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error opening file: %v", err)
		return
	}
	defer f.Close()
	f.WriteString(productInfoID + "\n")
}

func contentHash(body []byte) string {
	h := md5.Sum(body)
	return hex.EncodeToString(h[:])
}

// --- CORE API FETCH (with ETag + gzip) ---
// Returns (products, responseBody, changed, error)
// "changed" is false if ETag matched (304 Not Modified) — means no new data
func fetchAPI(apiURL string) ([]Product, []byte, bool, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, nil, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	req.Header.Set("Referer", "https://www.firstcry.com/")
	req.Header.Set("Connection", "keep-alive")

	// Add ETag for conditional request
	etagCacheMu.RLock()
	if etag, ok := etagCache[apiURL]; ok {
		req.Header.Set("If-None-Match", etag)
	}
	etagCacheMu.RUnlock()

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, nil, false, err
	}
	defer resp.Body.Close()

	// 304 Not Modified — content hasn't changed, skip parsing entirely
	if resp.StatusCode == 304 {
		return nil, nil, false, nil
	}

	if resp.StatusCode != 200 {
		return nil, nil, false, fmt.Errorf("bad status: %d", resp.StatusCode)
	}

	// Cache the new ETag
	if newEtag := resp.Header.Get("ETag"); newEtag != "" {
		etagCacheMu.Lock()
		etagCache[apiURL] = newEtag
		etagCacheMu.Unlock()
	}

	// Read body (handle gzip)
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, nil, false, fmt.Errorf("gzip error: %v", err)
		}
		defer gz.Close()
		reader = gz
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, false, err
	}

	// Parse
	var outer OuterEnvelope
	if err := json.Unmarshal(body, &outer); err != nil {
		var altOuter map[string]interface{}
		if err2 := json.Unmarshal(body, &altOuter); err2 == nil {
			if respStr, ok := altOuter["ProductResponse"].(string); ok {
				outer.ProductResponse = respStr
			} else {
				return nil, body, true, err
			}
		} else {
			return nil, body, true, err
		}
	}
	if outer.ProductResponse == "" {
		return []Product{}, body, true, nil
	}
	var inner InnerData
	if err := json.Unmarshal([]byte(outer.ProductResponse), &inner); err != nil {
		return nil, body, true, err
	}
	return inner.Products, body, true, nil
}

// --- PROCESS PRODUCTS (detect + notify INSTANTLY) ---
func processProducts(products []Product, source string) []Product {
	var newFound []Product
	for _, p := range products {
		if p.StockStatus == "0" {
			continue
		}
		uniqueID := p.ProductInfoID

		mutex.Lock()
		seen := seenItems[uniqueID]
		mutex.Unlock()

		if !seen {
			log.Printf("🚨 NEW ITEM [%s]: %s (₹%s)", source, p.ProductName, p.Price)
			newFound = append(newFound, p)

			fullURL := constructFullURL(p)
			message := fmt.Sprintf(
				"<b>🔥 New Hot Wheels Listing!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> ₹%s\n\n<b>Link:</b> <a href='%s'>Buy Now →</a>",
				p.ProductName, p.Price, fullURL,
			)
			// URGENT — direct goroutine, bypasses queue for minimum latency
			sendTelegramUrgent(TELEGRAM_CHAT_ID, message)

			// 🛒 AUTO ADD TO CART — fires in parallel, doesn't block anything
			go addToCart(p)

			saveNewItem(uniqueID)
			mutex.Lock()
			seenItems[uniqueID] = true
			totalNewItems++
			mutex.Unlock()
		}
	}
	return newFound
}

// --- ULTRA-FAST CHECK: 5 items, content-hash dedup ---
func ultraFastCheck() []Product {
	start := time.Now()
	mutex.Lock()
	totalChecks++
	mutex.Unlock()

	products, body, changed, err := fetchAPI(API_URL_ULTRA_FAST)
	if err != nil {
		log.Printf("❌ Ultra-fast error: %v", err)
		return nil
	}

	// If ETag said nothing changed, skip entirely
	if !changed {
		mutex.Lock()
		skippedChecks++
		mutex.Unlock()
		elapsed := time.Since(start)
		log.Printf("⚡ Ultra-fast: 304/no change (%dms) — SKIPPED", elapsed.Milliseconds())
		return nil
	}

	// Content hash: even if ETag wasn't supported, check if the actual data changed
	if body != nil {
		hash := contentHash(body)
		lastContentHashMu.Lock()
		if hash == lastContentHash {
			lastContentHashMu.Unlock()
			mutex.Lock()
			skippedChecks++
			mutex.Unlock()
			elapsed := time.Since(start)
			log.Printf("⚡ Ultra-fast: content unchanged (%dms) — SKIPPED", elapsed.Milliseconds())
			return nil
		}
		lastContentHash = hash
		lastContentHashMu.Unlock()
	}

	newItems := processProducts(products, "ULTRA")
	elapsed := time.Since(start)
	log.Printf("⚡ Ultra-fast done in %dms — %d items, %d new", elapsed.Milliseconds(), len(products), len(newItems))
	return newItems
}

// --- FAST CHECK: 20 items ---
func fastCheck() []Product {
	start := time.Now()
	mutex.Lock()
	totalChecks++
	mutex.Unlock()

	products, _, changed, err := fetchAPI(API_URL_FAST)
	if err != nil {
		log.Printf("❌ Fast error: %v", err)
		return nil
	}
	if !changed {
		mutex.Lock()
		skippedChecks++
		mutex.Unlock()
		elapsed := time.Since(start)
		log.Printf("⚡ Fast: no change (%dms) — SKIPPED", elapsed.Milliseconds())
		return nil
	}

	newItems := processProducts(products, "FAST")
	elapsed := time.Since(start)
	log.Printf("⚡ Fast done in %dms — %d items, %d new", elapsed.Milliseconds(), len(products), len(newItems))
	return newItems
}

// --- FULL SCAN: All pages parallel ---
func fullScan() []Product {
	start := time.Now()
	mutex.Lock()
	totalChecks++
	mutex.Unlock()
	log.Println("🔍 Full scan starting...")

	var allProducts []Product
	var mu sync.Mutex
	var wg sync.WaitGroup
	seenPInfIDs := make(map[string]bool)

	wg.Add(1)
	go func() {
		defer wg.Done()
		products, _, _, err := fetchAPI(API_URL_PAGE_1)
		if err != nil {
			log.Printf("❌ Full scan page 1 error: %v", err)
			return
		}
		mu.Lock()
		for _, p := range products {
			if !seenPInfIDs[p.ProductInfoID] {
				allProducts = append(allProducts, p)
				seenPInfIDs[p.ProductInfoID] = true
			}
		}
		mu.Unlock()
	}()

	for i := 2; i <= PAGES_TO_SCAN; i++ {
		wg.Add(1)
		go func(pageNum int) {
			defer wg.Done()
			pagingURL := fmt.Sprintf(API_URL_PAGING_TEMPLATE, pageNum)
			products, _, _, err := fetchAPI(pagingURL)
			if err != nil {
				log.Printf("❌ Full scan page %d error: %v", pageNum, err)
				return
			}
			mu.Lock()
			for _, p := range products {
				if !seenPInfIDs[p.ProductInfoID] {
					allProducts = append(allProducts, p)
					seenPInfIDs[p.ProductInfoID] = true
				}
			}
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	newItems := processProducts(allProducts, "FULL")
	elapsed := time.Since(start)
	log.Printf("🔍 Full scan done in %dms — %d unique, %d new", elapsed.Milliseconds(), len(allProducts), len(newItems))
	return newItems
}

// --- TRIPLE-LOOP SCRAPER ENGINE ---
func scraperWorker(stop chan struct{}) {
	// Initial checks
	initialFinds := fastCheck()
	recordHistory(initialFinds)
	if len(initialFinds) == 0 {
		log.Println("... No new items on startup.")
	}

	fullFinds := fullScan()
	recordHistory(fullFinds)

	// Three concurrent poll loops:
	// 1. Ultra-fast (3s) — 5 items, content-hash dedup, near-zero overhead when unchanged
	// 2. Fast (10s) — 20 items, catches items that might be at position 6-20
	// 3. Full (90s) — all pages, safety net

	ultraTicker := time.NewTicker(ultraFastInterval)
	fastTicker := time.NewTicker(FAST_POLL_INTERVAL)
	fullTicker := time.NewTicker(fullScanInterval)
	defer ultraTicker.Stop()
	defer fastTicker.Stop()
	defer fullTicker.Stop()

	// Cycle counter for ultra-fast to alternate with fast
	for {
		select {
		case <-ultraTicker.C:
			mutex.Lock()
			paused := isPaused
			currentInterval := ultraFastInterval
			mutex.Unlock()

			if currentInterval != ultraFastInterval {
				ultraTicker.Reset(currentInterval)
			}

			if !paused {
				newItems := ultraFastCheck()
				recordHistory(newItems)
			}

		case <-fastTicker.C:
			mutex.Lock()
			paused := isPaused
			mutex.Unlock()

			if !paused {
				newItems := fastCheck()
				recordHistory(newItems)
			}

		case <-fullTicker.C:
			mutex.Lock()
			paused := isPaused
			isMuted := heartbeatMuted
			mutex.Unlock()

			if !paused {
				newItems := fullScan()
				recordHistory(newItems)

				if len(newItems) == 0 && !isMuted {
					mutex.Lock()
					stats := fmt.Sprintf("✅ Full scan done. No new items.\n⚡ Ultra-fast every %ds | Full every %ds\n📊 Checks: %d | Skipped: %d | New: %d",
						int(ultraFastInterval.Seconds()), int(fullScanInterval.Seconds()),
						totalChecks, skippedChecks, totalNewItems)
					mutex.Unlock()
					sendTelegramMessage(TELEGRAM_CHAT_ID, stats)
				}
			}

		case <-stop:
			log.Println("Scraper shutting down.")
			return
		}
	}
}

func recordHistory(items []Product) {
	mutex.Lock()
	checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: items})
	if len(checkHistory) > 30 {
		checkHistory = checkHistory[1:]
	}
	mutex.Unlock()
}

// --- RESTOCK MONITOR: Polls specific products, INSTANT cart on restock ---
func restockMonitorWorker(stop chan struct{}) {
	restockWatchlistMu.RLock()
	if !restockWatchlist.Enabled || len(restockWatchlist.Products) == 0 {
		restockWatchlistMu.RUnlock()
		log.Println("👁️ Restock monitor: nothing to watch, sleeping...")
		// Still stay alive — watchlist can be updated via commands
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Second):
				restockWatchlistMu.RLock()
				hasProducts := restockWatchlist.Enabled && len(restockWatchlist.Products) > 0
				restockWatchlistMu.RUnlock()
				if hasProducts {
					log.Println("👁️ Restock monitor: watchlist updated, starting monitoring...")
					goto startMonitoring
				}
			}
		}
	}
	restockWatchlistMu.RUnlock()

startMonitoring:
	log.Println("👁️ Restock monitor ACTIVE — watching for stock changes...")

	restockWatchlistMu.RLock()
	interval := time.Duration(restockWatchlist.PollIntervalSecs) * time.Second
	if interval < 1*time.Second {
		interval = RESTOCK_POLL_INTERVAL
	}
	restockWatchlistMu.RUnlock()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			log.Println("👁️ Restock monitor shutting down.")
			return
		case <-ticker.C:
			mutex.Lock()
			paused := isPaused
			mutex.Unlock()
			if paused {
				continue
			}

			restockWatchlistMu.RLock()
			if !restockWatchlist.Enabled || len(restockWatchlist.Products) == 0 {
				restockWatchlistMu.RUnlock()
				continue
			}
			// Copy products to check
			productsToCheck := make([]WatchProduct, len(restockWatchlist.Products))
			copy(productsToCheck, restockWatchlist.Products)
			restockWatchlistMu.RUnlock()

			// Bounded concurrency — max RESTOCK_MAX_CONCURRENT simultaneous checks
			sem := make(chan struct{}, RESTOCK_MAX_CONCURRENT)
			for _, wp := range productsToCheck {
				sem <- struct{}{} // Acquire slot
				go func(w WatchProduct) {
					defer func() { <-sem }() // Release slot
					checkRestockAndCart(w)
				}(wp)
			}
		}
	}
}

// Check a single product's stock and INSTANTLY add to cart if restocked
func checkRestockAndCart(wp WatchProduct) {
	start := time.Now()

	// Use the search API with ProductidQstr to query this specific product
	checkURL := fmt.Sprintf(
		"https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=1&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=%s&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype=",
		wp.ProductID,
	)

	products, _, _, err := fetchAPI(checkURL)
	if err != nil {
		log.Printf("👁️ Restock check error for %s: %v", wp.Name, err)
		return
	}

	elapsed := time.Since(start)

	if len(products) == 0 {
		log.Printf("👁️ Restock check: %s — not found in API (%dms)", wp.Name, elapsed.Milliseconds())
		return
	}

	p := products[0]
	currentStock := p.StockStatus

	// RESTOCK DETECTED: was out of stock ("0"), now in stock!
	if currentStock != "0" && (wp.LastStock == "0" || wp.LastStock == "") {
		log.Printf("🚨🚨🚨 RESTOCK DETECTED: %s (stock: %s) — ADDING TO CART INSTANTLY!", wp.Name, currentStock)

		// 🛒 CART FIRST — absolute priority, zero delay
		go addToCart(p)

		// 📢 Then notify
		productURL := wp.URL
		if productURL == "" {
			productURL = constructFullURL(p)
		}
		sendTelegramUrgent(TELEGRAM_CHAT_ID, fmt.Sprintf(
			"🚨🔥 <b>RESTOCKED! ADDING TO CART!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> ₹%s\n<b>Stock:</b> %s\n⚡ Detected in %dms\n\n<a href='%s'>🛒 View/Buy →</a>",
			p.ProductName, p.Price, currentStock, elapsed.Milliseconds(), productURL,
		))

		// Remove from watchlist after successful restock detection
		restockWatchlistMu.Lock()
		for i, rwp := range restockWatchlist.Products {
			if rwp.ProductID == wp.ProductID {
				restockWatchlist.Products = append(restockWatchlist.Products[:i], restockWatchlist.Products[i+1:]...)
				break
			}
		}
		restockWatchlistMu.Unlock()
		saveRestockWatchlist()
		log.Printf("👁️ Removed %s from watchlist (restocked!)", wp.Name)

	} else if currentStock == "0" {
		log.Printf("👁️ Still OOS: %s (%dms)", wp.Name, elapsed.Milliseconds())
		// Update last known stock
		restockWatchlistMu.Lock()
		for i, rwp := range restockWatchlist.Products {
			if rwp.ProductID == wp.ProductID {
				restockWatchlist.Products[i].LastStock = currentStock
				break
			}
		}
		restockWatchlistMu.Unlock()
	} else {
		log.Printf("👁️ In stock: %s (stock: %s, %dms)", wp.Name, currentStock, elapsed.Milliseconds())
	}
}

// --- TELEGRAM COMMAND LISTENER ---
func commandListenerWorker(stop chan struct{}) {
	log.Println("🤖 Command listener started.")
	var lastUpdateID int
	for {
		select {
		case <-stop:
			return
		default:
		}

		apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=5", TELEGRAM_BOT_TOKEN, lastUpdateID+1)
		resp, err := telegramClient.Get(apiURL)
		if err != nil {
			log.Printf("Error getting updates: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdateResponse
		json.Unmarshal(body, &updates)

		for _, update := range updates.Result {
			lastUpdateID = update.UpdateID
			if update.Message.Text == "" || update.Message.Chat.ID == 0 {
				continue
			}
			chatIDStr := strconv.FormatInt(update.Message.Chat.ID, 10)
			if chatIDStr != ADMIN_CHAT_ID {
				sendTelegramMessage(chatIDStr, "Sorry, you are not authorized.")
				continue
			}

			parts := strings.Fields(update.Message.Text)
			command := parts[0]

			switch command {
			case "/start":
				mutex.Lock()
				isPaused = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "▶️ Bot resumed.")

			case "/pause":
				mutex.Lock()
				isPaused = true
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "⏸️ Bot paused.")

			case "/stop":
				sendTelegramMessage(ADMIN_CHAT_ID, "🛑 Stopping bot...")
				time.Sleep(500 * time.Millisecond)
				close(stop)
				return

			case "/mute":
				mutex.Lock()
				heartbeatMuted = true
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔕 Heartbeat muted.")

			case "/unmute":
				mutex.Lock()
				heartbeatMuted = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔔 Heartbeat enabled.")

			case "/setinterval":
				if len(parts) > 1 {
					i, err := strconv.Atoi(parts[1])
					if err == nil && i >= 1 {
						mutex.Lock()
						ultraFastInterval = time.Duration(i) * time.Second
						mutex.Unlock()
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("✅ Ultra-fast interval set to %d seconds.", i))
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, "❌ Invalid interval. Min: 1 second.")
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /setinterval <seconds>")
				}

			case "/setfull":
				if len(parts) > 1 {
					i, err := strconv.Atoi(parts[1])
					if err == nil && i >= 10 {
						mutex.Lock()
						fullScanInterval = time.Duration(i) * time.Second
						mutex.Unlock()
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("✅ Full scan interval set to %d seconds.", i))
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, "❌ Invalid interval. Min: 10 seconds.")
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /setfull <seconds>")
				}

			case "/status":
				mutex.Lock()
				status := "▶️ Running"
				if isPaused {
					status = "⏸️ Paused"
				}
				hbStatus := "🔔 Active"
				if heartbeatMuted {
					hbStatus = "🔕 Muted"
				}
				uInterval := ultraFastInterval
				sInterval := fullScanInterval
				itemCount := len(seenItems)
				checks := totalChecks
				skipped := skippedChecks
				newItems := totalNewItems
				mutex.Unlock()
				uptime := time.Since(startTime).Round(time.Second)
				sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf(
					"<b>⚡ Bot Status (TURBO)</b>\n%s\n\n<b>Speed:</b>\nUltra-fast: every %ds (5 items)\nFast: every 10s (20 items)\nFull scan: every %ds (all pages)\n\n<b>Stats:</b>\nUptime: %s\nChecks: %d | Skipped: %d\nNew items found: %d\nTracked items: %d\nHeartbeat: %s",
					status, int(uInterval.Seconds()), int(sInterval.Seconds()),
					uptime.String(), checks, skipped, newItems, itemCount, hbStatus,
				))

			case "/recent":
				var sb strings.Builder
				sb.WriteString("<b>🔎 Recent Finds</b>\n\n")
				mutex.Lock()
				totalFound := 0
				for i := len(checkHistory) - 1; i >= 0; i-- {
					result := checkHistory[i]
					if len(result.FoundProducts) > 0 {
						totalFound += len(result.FoundProducts)
						loc, _ := time.LoadLocation("Asia/Kolkata")
						sb.WriteString(fmt.Sprintf("<b><u>Found at %s:</u></b>\n", result.Timestamp.In(loc).Format("03:04 PM, Jan 02")))
						for _, p := range result.FoundProducts {
							fullURL := constructFullURL(p)
							sb.WriteString(fmt.Sprintf("- <a href='%s'>%s</a>\n", fullURL, p.ProductName))
						}
						sb.WriteString("\n")
					}
				}
				mutex.Unlock()
				if totalFound == 0 {
					sb.WriteString("No new products found in recent checks.")
				}
				sendTelegramMessage(ADMIN_CHAT_ID, sb.String())

			case "/help":
				sendTelegramMessage(ADMIN_CHAT_ID, "<b>⚡ Commands</b>\n"+
					"/status — Full status + stats\n"+
					"/setinterval N — Ultra-fast poll interval (seconds)\n"+
					"/setfull N — Full scan interval (seconds)\n"+
					"/pause — Pause all monitoring\n"+
					"/start — Resume monitoring\n"+
					"/mute — Mute heartbeat messages\n"+
					"/unmute — Enable heartbeat messages\n"+
					"/recent — Show recent finds\n"+
					"/stop — Shutdown bot\n\n"+
					"<b>🔄 Restock Watchlist:</b>\n"+
					"/watch URL — Watch product for restock\n"+
					"/unwatch ID — Stop watching product\n"+
					"/watchlist — Show watched products\n"+
					"/restock_on — Enable restock monitor\n"+
					"/restock_off — Disable restock monitor")

			case "/cart_on":
				cartConfigMu.Lock()
				cartConfig.Enabled = true
				cartConfigMu.Unlock()
				saveCartConfig()
				sendTelegramMessage(ADMIN_CHAT_ID, "🛒 Auto add-to-cart ENABLED.")

			case "/cart_off":
				cartConfigMu.Lock()
				cartConfig.Enabled = false
				cartConfigMu.Unlock()
				saveCartConfig()
				sendTelegramMessage(ADMIN_CHAT_ID, "🛒 Auto add-to-cart DISABLED.")

			case "/updatecookies":
				if len(parts) > 1 {
					newCookies := strings.Join(parts[1:], " ")
					cartConfigMu.Lock()
					cartConfig.Cookies = newCookies
					// Auto-extract FC_AUTH as FTK — saves a separate /updateftk step
					extractedFtk := extractFtkFromCookies(newCookies)
					if extractedFtk != "" {
						cartConfig.Ftk = extractedFtk
					}
					cartConfigMu.Unlock()
					saveCartConfig()
					if extractedFtk != "" {
						sendTelegramMessage(ADMIN_CHAT_ID, "✅ Cart cookies updated + FTK auto-extracted from FC_AUTH!")
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, "✅ Cart cookies updated. (⚠️ FC_AUTH not found — use /updateftk manually)")
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /updatecookies <cookie_string>")
				}

			case "/updateftk":
				if len(parts) > 1 {
					newFtk := parts[1]
					cartConfigMu.Lock()
					cartConfig.Ftk = newFtk
					cartConfigMu.Unlock()
					saveCartConfig()
					sendTelegramMessage(ADMIN_CHAT_ID, "✅ Cart FTK token updated.")
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /updateftk <token>")
				}

			case "/watch":
				if len(parts) > 1 {
					rawURL := parts[1]
					pid := extractProductIDFromURL(rawURL)
					if pid == "" {
						// Maybe they just passed a product ID directly
						if _, err := strconv.Atoi(rawURL); err == nil {
							pid = rawURL
						}
					}
					if pid == "" {
						sendTelegramMessage(ADMIN_CHAT_ID, "❌ Could not extract product ID from URL.\nUsage: /watch <firstcry_product_url>")
					} else {
						name := extractNameFromURL(rawURL)
						if len(parts) > 2 {
							name = strings.Join(parts[2:], " ")
						}
						restockWatchlistMu.Lock()
						// Check if already watching
						alreadyWatching := false
						for _, wp := range restockWatchlist.Products {
							if wp.ProductID == pid {
								alreadyWatching = true
								break
							}
						}
						if alreadyWatching {
							restockWatchlistMu.Unlock()
							sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("⚠️ Already watching product %s", pid))
						} else {
							restockWatchlist.Products = append(restockWatchlist.Products, WatchProduct{
								ProductID: pid,
								Name:      name,
								URL:       rawURL,
								LastStock: "0",
							})
							restockWatchlist.Enabled = true
							restockWatchlistMu.Unlock()
							saveRestockWatchlist()
							sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("👁️ Now watching for restock:\n<b>%s</b>\nID: %s\n\nPolling every %ds", name, pid, restockWatchlist.PollIntervalSecs))
						}
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /watch <firstcry_product_url> [optional name]")
				}

			case "/unwatch":
				if len(parts) > 1 {
					pid := parts[1]
					// Also try extracting from URL
					if extracted := extractProductIDFromURL(pid); extracted != "" {
						pid = extracted
					}
					restockWatchlistMu.Lock()
					found := false
					for i, wp := range restockWatchlist.Products {
						if wp.ProductID == pid {
							restockWatchlist.Products = append(restockWatchlist.Products[:i], restockWatchlist.Products[i+1:]...)
							found = true
							break
						}
					}
					restockWatchlistMu.Unlock()
					if found {
						saveRestockWatchlist()
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("✅ Stopped watching product %s", pid))
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("❌ Product %s not in watchlist", pid))
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /unwatch <product_id_or_url>")
				}

			case "/watchlist":
				restockWatchlistMu.RLock()
				if len(restockWatchlist.Products) == 0 {
					restockWatchlistMu.RUnlock()
					sendTelegramMessage(ADMIN_CHAT_ID, "👁️ Restock watchlist is empty.\nUse /watch <url> to add products.")
				} else {
					var sb strings.Builder
					sb.WriteString(fmt.Sprintf("<b>👁️ Restock Watchlist</b> (%s)\n\n", map[bool]string{true: "✅ Active", false: "⏸ Disabled"}[restockWatchlist.Enabled]))
					for i, wp := range restockWatchlist.Products {
						stockEmoji := "🔴"
						if wp.LastStock != "0" && wp.LastStock != "" {
							stockEmoji = "🟢"
						}
						sb.WriteString(fmt.Sprintf("%d. %s <b>%s</b>\n   ID: %s\n", i+1, stockEmoji, wp.Name, wp.ProductID))
						if wp.URL != "" {
							sb.WriteString(fmt.Sprintf("   <a href='%s'>🔗 Link</a>\n", wp.URL))
						}
					}
					sb.WriteString(fmt.Sprintf("\n⏱ Polling every %ds", restockWatchlist.PollIntervalSecs))
					restockWatchlistMu.RUnlock()
					sendTelegramMessage(ADMIN_CHAT_ID, sb.String())
				}

			case "/restock_on":
				restockWatchlistMu.Lock()
				restockWatchlist.Enabled = true
				restockWatchlistMu.Unlock()
				saveRestockWatchlist()
				sendTelegramMessage(ADMIN_CHAT_ID, "👁️ Restock monitor ENABLED.")

			case "/restock_off":
				restockWatchlistMu.Lock()
				restockWatchlist.Enabled = false
				restockWatchlistMu.Unlock()
				saveRestockWatchlist()
				sendTelegramMessage(ADMIN_CHAT_ID, "👁️ Restock monitor DISABLED.")
			}
		}
	}
}

// --- BASELINE ---
func initializeBaseline() {
	log.Println("No baseline found. Running initial full scan...")
	products, err := fullScanProducts()
	if err != nil {
		log.Printf("❌ Baseline error: %v", err)
		return
	}
	var items []string
	for _, p := range products {
		if p.StockStatus != "0" {
			items = append(items, p.ProductInfoID)
		}
	}
	os.WriteFile(SEEN_ITEMS_FILE, []byte(strings.Join(items, "\n")), 0644)
	log.Printf("✅ Baseline: %d in-stock items.", len(items))
}

func fullScanProducts() ([]Product, error) {
	var allProducts []Product
	var mu sync.Mutex
	var wg sync.WaitGroup
	seenPInfIDs := make(map[string]bool)

	wg.Add(1)
	go func() {
		defer wg.Done()
		products, _, _, err := fetchAPI(API_URL_PAGE_1)
		if err != nil {
			log.Printf("❌ Baseline page 1 error: %v", err)
			return
		}
		mu.Lock()
		for _, p := range products {
			if !seenPInfIDs[p.ProductInfoID] {
				allProducts = append(allProducts, p)
				seenPInfIDs[p.ProductInfoID] = true
			}
		}
		mu.Unlock()
	}()

	for i := 2; i <= PAGES_TO_SCAN; i++ {
		wg.Add(1)
		go func(pageNum int) {
			defer wg.Done()
			products, _, _, err := fetchAPI(fmt.Sprintf(API_URL_PAGING_TEMPLATE, pageNum))
			if err != nil {
				log.Printf("❌ Baseline page %d error: %v", pageNum, err)
				return
			}
			mu.Lock()
			for _, p := range products {
				if !seenPInfIDs[p.ProductInfoID] {
					allProducts = append(allProducts, p)
					seenPInfIDs[p.ProductInfoID] = true
				}
			}
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	return allProducts, nil
}

// --- EXTRACT FTK FROM COOKIES ---
func extractFtkFromCookies(cookies string) string {
	// Look for FC_AUTH=<value> in the cookie string
	for _, pair := range strings.Split(cookies, ";") {
		pair = strings.TrimSpace(pair)
		if strings.HasPrefix(pair, "FC_AUTH=") {
			val := strings.TrimPrefix(pair, "FC_AUTH=")
			// URL-decode if needed
			if decoded, err := url.QueryUnescape(val); err == nil {
				return decoded
			}
			return val
		}
	}
	return ""
}

// --- PRE-WARM CONNECTIONS (eliminates cold-start latency) ---
func preWarmConnections() {
	log.Println("🔥 Pre-warming connections...")
	var wg sync.WaitGroup

	// Warm FirstCry API connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("HEAD", "https://www.firstcry.com/", nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		if resp, err := apiClient.Do(req); err == nil {
			resp.Body.Close()
			log.Println("  ✅ FirstCry API connection warmed")
		} else {
			log.Printf("  ⚠️ FirstCry warm failed (ok): %v", err)
		}
	}()

	// Warm Cart API connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("HEAD", "https://www.firstcry.com/svcs/CommonService.svc/", nil)
		req.Header.Set("User-Agent", "Mozilla/5.0")
		if resp, err := cartClient.Do(req); err == nil {
			resp.Body.Close()
			log.Println("  ✅ Cart API connection warmed")
		} else {
			log.Printf("  ⚠️ Cart warm failed (ok): %v", err)
		}
	}()

	// Warm Telegram API connection
	wg.Add(1)
	go func() {
		defer wg.Done()
		testURL := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", TELEGRAM_BOT_TOKEN)
		if resp, err := telegramClient.Get(testURL); err == nil {
			resp.Body.Close()
			log.Println("  ✅ Telegram API connection warmed")
		} else {
			log.Printf("  ⚠️ Telegram warm failed (ok): %v", err)
		}
	}()

	wg.Wait()
	log.Println("🔥 All connections pre-warmed!")
}

// --- KEEP-ALIVE ---
func startKeepAlive() {
	appURL := "https://hh-mvnn.onrender.com"
	go func() {
		time.Sleep(2 * time.Minute)
		ticker := time.NewTicker(8 * time.Minute)
		defer ticker.Stop()
		log.Printf("🔄 Keep-alive: %s", appURL)
		for range ticker.C {
			resp, err := apiClient.Get(appURL + "/ping")
			if err != nil {
				log.Printf("⚠️ Keep-alive failed: %v", err)
			} else {
				resp.Body.Close()
			}
		}
	}()
}

// --- MAIN ---
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	startTime = time.Now()
	log.Println("--- ⚡ Hot Wheels Hunter TURBO v3 ⚡ ---")
	log.Printf("Speed config: Ultra=%ds | Fast=%ds | Full=%ds | Restock=%ds", int(ULTRA_FAST_INTERVAL.Seconds()), int(FAST_POLL_INTERVAL.Seconds()), int(FULL_SCAN_INTERVAL.Seconds()), int(RESTOCK_POLL_INTERVAL.Seconds()))

	// Pre-warm all connections before anything starts
	preWarmConnections()

	// 5 concurrent Telegram senders for max throughput (was 3)
	for i := 0; i < 5; i++ {
		go telegramSenderWorker()
	}

	// HTTP server for Render
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}

		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("⚡ Hot Wheels Hunter TURBO v3 is running! ⚡"))
		})

		http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			mutex.Lock()
			status := "running"
			if isPaused {
				status = "paused"
			}
			uInterval := ultraFastInterval
			sInterval := fullScanInterval
			itemCount := len(seenItems)
			checks := totalChecks
			skipped := skippedChecks
			newItems := totalNewItems
			mutex.Unlock()
			uptime := time.Since(startTime).Round(time.Second)

			response := fmt.Sprintf(`{
				"status": "%s",
				"ultra_fast_seconds": %.0f,
				"full_scan_seconds": %.0f,
				"tracked_items": %d,
				"total_checks": %d,
				"skipped_checks": %d,
				"new_items_found": %d,
				"uptime": "%s",
				"bot": "Hot Wheels Hunter TURBO v3",
				"timestamp": "%s"
			}`, status, uInterval.Seconds(), sInterval.Seconds(), itemCount,
				checks, skipped, newItems, uptime.String(),
				time.Now().Format("2006-01-02 15:04:05"))

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(response))
		})

		http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		})

		log.Printf("🌐 HTTP on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("❌ HTTP error: %v", err)
		}
	}()

	startKeepAlive()
	loadCartConfig()
	loadRestockWatchlist()

	if _, err := os.Stat(SEEN_ITEMS_FILE); os.IsNotExist(err) {
		initializeBaseline()
	}
	loadSeenItems()
	log.Printf("✅ Loaded %d tracked items.", len(seenItems))

	stop := make(chan struct{})
	go scraperWorker(stop)
	go commandListenerWorker(stop)
	go restockMonitorWorker(stop)

	restockWatchlistMu.RLock()
	restockStatus := "DISABLED"
	restockCount := len(restockWatchlist.Products)
	if restockWatchlist.Enabled && restockCount > 0 {
		restockStatus = fmt.Sprintf("WATCHING %d products", restockCount)
	}
	restockWatchlistMu.RUnlock()

	sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf(
		"⚡ <b>Bot TURBO v3 Online!</b>\n\nUltra-fast: every %ds (5 items)\nFast: every %ds (20 items)\nFull scan: every %ds (all pages)\n👁️ Restock monitor: %s\n\n🔥 Pre-warmed + Aggressive Keep-Alive + Auto-FTK active",
		int(ULTRA_FAST_INTERVAL.Seconds()), int(FAST_POLL_INTERVAL.Seconds()), int(FULL_SCAN_INTERVAL.Seconds()), restockStatus))

	<-stop
	log.Println("--- Bot shut down. ---")
}
