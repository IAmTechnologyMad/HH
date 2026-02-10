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

	PAGES_TO_SCAN      = 6
	TELEGRAM_BOT_TOKEN = "8336369415:AAE7idSEyOpMIUlYhL4z9yze0C4_6rdbzE4"
	TELEGRAM_CHAT_ID   = "-4985438208"
	ADMIN_CHAT_ID      = "837428747"
	SEEN_ITEMS_FILE    = "seen_hotwheels_go.txt"
	CART_CONFIG_FILE   = "cart_config.json"

	// --- CART API ---
	CART_API_URL  = "https://www.firstcry.com/svcs/CommonService.svc/SaveCartDetail"
	CART_SAVE_URL = "https://www.firstcry.com/capinet/pdp/SaveProductCart"

	// --- SPEED TUNING ---
	ULTRA_FAST_INTERVAL = 3 * time.Second  // Tiny 5-item check every 3 seconds
	FAST_POLL_INTERVAL  = 10 * time.Second // 20-item check every 10 seconds (alternates with ultra)
	FULL_SCAN_INTERVAL  = 90 * time.Second // Full 6-page scan every 90 seconds
	HTTP_TIMEOUT        = 4 * time.Second
	TELEGRAM_TIMEOUT    = 5 * time.Second
)

// --- PERSISTENT HTTP CLIENTS (connection reuse) ---
var (
	apiClient = &http.Client{
		Timeout: HTTP_TIMEOUT,
		Transport: &http.Transport{
			MaxIdleConns:        15,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     120 * time.Second,
			TLSHandshakeTimeout: 3 * time.Second,
			DisableCompression:  false,
			// ForceAttemptHTTP2 ensures HTTP/2 is used when available
			ForceAttemptHTTP2: true,
		},
	}

	telegramClient = &http.Client{
		Timeout: TELEGRAM_TIMEOUT,
		Transport: &http.Transport{
			MaxIdleConns:        5,
			MaxIdleConnsPerHost: 3,
			IdleConnTimeout:     120 * time.Second,
			TLSHandshakeTimeout: 3 * time.Second,
			ForceAttemptHTTP2:   true,
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
)

// --- CART CONFIG ---
type CartConfig struct {
	Enabled bool   `json:"enabled"`
	Cookies string `json:"cookies"`
	Ftk     string `json:"ftk"`
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

// --- ADD TO CART (fires instantly, ~200ms) ---
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

	start := time.Now()

	// Request 1: SaveCartDetail (primary add-to-cart)
	payload1 := fmt.Sprintf(`{"ftk":%q,"viewid":"","productid":%q,"quantity":"1","offertype":"NO","offerid":"","action":"add","gcoffer":""}`, ftk, p.ProductID)

	req1, err := http.NewRequest("POST", CART_API_URL, bytes.NewBufferString(payload1))
	if err != nil {
		log.Printf("❌ Cart req1 build error: %v", err)
		return
	}
	req1.Header.Set("Content-Type", "application/json; charset=utf-8")
	req1.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req1.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req1.Header.Set("X-Requested-With", "XMLHttpRequest")
	req1.Header.Set("Origin", "https://www.firstcry.com")
	req1.Header.Set("Referer", "https://www.firstcry.com/")
	req1.Header.Set("Cookie", cookies)

	resp1, err := apiClient.Do(req1)
	if err != nil {
		log.Printf("❌ Cart req1 failed: %v", err)
		sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("❌ Failed to add to cart: %s\nError: %v", p.ProductName, err))
		return
	}
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()

	elapsed := time.Since(start)

	if resp1.StatusCode == 200 {
		log.Printf("🛒✅ ADDED TO CART in %dms: %s (PID: %s)", elapsed.Milliseconds(), p.ProductName, p.ProductID)
		mutex.Lock()
		totalCarted++
		mutex.Unlock()
		sendTelegramUrgent(TELEGRAM_CHAT_ID, fmt.Sprintf(
			"🛒 <b>Auto-Added to Cart!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> ₹%s\n⚡ Added in %dms",
			p.ProductName, p.Price, elapsed.Milliseconds(),
		))

		// Request 2: SaveProductCart (secondary confirmation)
		go func() {
			payload2 := fmt.Sprintf(`{"objdd":{"ProductID":%q,"ViewID":"","ProductType":"product","cart":"cart","Discount":""},"ftk":%q}`, p.ProductID, ftk)
			req2, _ := http.NewRequest("POST", CART_SAVE_URL, bytes.NewBufferString(payload2))
			req2.Header.Set("Content-Type", "application/json; charset=utf-8")
			req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
			req2.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
			req2.Header.Set("X-Requested-With", "XMLHttpRequest")
			req2.Header.Set("Origin", "https://www.firstcry.com")
			req2.Header.Set("Referer", "https://www.firstcry.com/")
			req2.Header.Set("Cookie", cookies)
			resp2, err := apiClient.Do(req2)
			if err != nil {
				log.Printf("⚠️ Cart save (secondary) failed: %v", err)
				return
			}
			resp2.Body.Close()
		}()
	} else {
		log.Printf("❌ Cart req1 bad status %d: %s", resp1.StatusCode, string(body1))
		sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("❌ Cart add failed (HTTP %d): %s\nResponse: %s", resp1.StatusCode, p.ProductName, string(body1)))
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
	data, _ := os.ReadFile(SEEN_ITEMS_FILE)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			seenItems[line] = true
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
					"/stop — Shutdown bot")

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
					cartConfigMu.Unlock()
					saveCartConfig()
					sendTelegramMessage(ADMIN_CHAT_ID, "✅ Cart cookies updated.")
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
	log.Println("--- ⚡ Hot Wheels Hunter TURBO v2 ⚡ ---")
	log.Printf("Speed config: Ultra=%ds | Fast=10s | Full=%ds", int(ULTRA_FAST_INTERVAL.Seconds()), int(FULL_SCAN_INTERVAL.Seconds()))

	// 3 concurrent Telegram senders for max throughput
	for i := 0; i < 3; i++ {
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
			w.Write([]byte("⚡ Hot Wheels Hunter TURBO v2 is running! ⚡"))
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
				"bot": "Hot Wheels Hunter TURBO v2",
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

	if _, err := os.Stat(SEEN_ITEMS_FILE); os.IsNotExist(err) {
		initializeBaseline()
	}
	loadSeenItems()
	log.Printf("✅ Loaded %d tracked items.", len(seenItems))

	stop := make(chan struct{})
	go scraperWorker(stop)
	go commandListenerWorker(stop)

	sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf(
		"⚡ <b>Bot TURBO v2 Online!</b>\n\nUltra-fast: every %ds (5 items)\nFast: every 10s (20 items)\nFull scan: every %ds (all pages)\n\n🔥 ETag caching + content-hash dedup active",
		int(ULTRA_FAST_INTERVAL.Seconds()), int(FULL_SCAN_INTERVAL.Seconds())))

	<-stop
	log.Println("--- Bot shut down. ---")
}
