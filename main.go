package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// WebCrawler represents the main crawler structure
type WebCrawler struct {
	maxDepth  int
	maxPages  int
	visited   map[string]bool
	graph     map[string][]string
	mutex     sync.Mutex
	client    *http.Client
	startTime time.Time
	deadLinks int
	verbose   bool
}

// crawlBatch for sending links with depth
type crawlBatch struct {
	depth int
	links []string
}

// PageRank represents a page with its rank score
type PageRank struct {
	URL           string
	Rank          float64
	InboundLinks  int
	OutboundLinks int
	Category      string
}

// CrawlStats holds crawling statistics
type CrawlStats struct {
	TotalPages      int
	TotalLinks      int
	CrawlDuration   time.Duration
	AvgLinksPerPage float64
	DeadLinks       int
	UniquePages     int
}

// NewWebCrawler creates a new crawler instance
func NewWebCrawler(maxDepth, maxPages int, timeout time.Duration, verbose bool) *WebCrawler {
	return &WebCrawler{
		maxDepth: maxDepth,
		maxPages: maxPages,
		visited:  make(map[string]bool),
		graph:    make(map[string][]string),
		client: &http.Client{
			Timeout: timeout,
		},
		startTime: time.Now(),
		deadLinks: 0,
		verbose:   verbose,
	}
}

// normalizeURL canonicalizes a URL for consistent storage
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// contains checks if a slice contains a string
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// printHeader displays a styled header
func printHeader(title string) {
	width := 80
	fmt.Println()
	fmt.Println(strings.Repeat("═", width))
	padding := (width - len(title)) / 2
	fmt.Printf("%s%s%s\n", strings.Repeat(" ", padding), title, strings.Repeat(" ", width-padding-len(title)))
	fmt.Println(strings.Repeat("═", width))
}

// printSection displays a section header
func printSection(title string) {
	fmt.Printf("\n🔹 %s\n", title)
	fmt.Println(strings.Repeat("─", len(title)+4))
}

// fetchPage fetches the HTML content of a URL
func (wc *WebCrawler) fetchPage(targetURL string) (string, error) {
	resp, err := wc.client.Get(targetURL)
	if err != nil {
		wc.mutex.Lock()
		wc.deadLinks++
		wc.mutex.Unlock()
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		wc.mutex.Lock()
		wc.deadLinks++
		wc.mutex.Unlock()
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// parseLinks extracts all links from HTML content
func (wc *WebCrawler) parseLinks(htmlContent, baseURL string) ([]string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return nil, fmt.Errorf("error parsing HTML: %v", err)
	}

	var links []string
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("error parsing base URL: %v", err)
	}

	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key == "href" {
					href := strings.TrimSpace(attr.Val)
					if href != "" && !strings.HasPrefix(href, "#") && !strings.HasPrefix(href, "mailto:") && !strings.HasPrefix(href, "tel:") {
						if parsed, err := url.Parse(href); err == nil {
							absolute := base.ResolveReference(parsed)
							if absolute.Scheme == "http" || absolute.Scheme == "https" {
								normalized := normalizeURL(absolute.String())
								links = append(links, normalized)
							}
						}
					}
					break
				}
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			traverse(child)
		}
	}

	traverse(doc)
	return links, nil
}

// crawlPage crawls a single page and extracts links
func (wc *WebCrawler) crawlPage(targetURL string, depth int, results chan<- crawlBatch, wg *sync.WaitGroup) {
	defer wg.Done()

	normalizedURL := normalizeURL(targetURL)

	wc.mutex.Lock()
	if wc.visited[normalizedURL] || len(wc.visited) >= wc.maxPages || depth > wc.maxDepth {
		wc.mutex.Unlock()
		return
	}
	wc.visited[normalizedURL] = true
	wc.mutex.Unlock()

	if wc.verbose {
		fmt.Printf("  📄 Crawling (depth %d): %s\n", depth, normalizedURL)
	}

	// Fetch the page content
	htmlContent, err := wc.fetchPage(normalizedURL)
	if err != nil {
		log.Printf("  ❌ Error fetching %s: %v", normalizedURL, err)
		return
	}

	// Parse links from the page
	links, parseErr := wc.parseLinks(htmlContent, normalizedURL)
	if parseErr != nil {
		log.Printf("  ❌ Error parsing links for %s: %v", normalizedURL, parseErr)
		return
	}

	// Filter links to same domain for focused crawling
	var filteredLinks []string
	baseURL, _ := url.Parse(normalizedURL)
	baseHost := baseURL.Host
	for _, link := range links {
		if linkURL, err := url.Parse(link); err == nil && linkURL.Host == baseHost {
			filteredLinks = append(filteredLinks, link)
		}
	}

	// Store the links in the graph
	wc.mutex.Lock()
	wc.graph[normalizedURL] = filteredLinks
	wc.mutex.Unlock()

	// Send links for further crawling
	if depth < wc.maxDepth {
		results <- crawlBatch{depth: depth + 1, links: filteredLinks}
	}
}

// Crawl performs the web crawling starting from a seed URL
func (wc *WebCrawler) Crawl(seedURL string) {
	printSection("WEB CRAWLING IN PROGRESS")
	fmt.Printf("  🌱 Seed URL: %s\n", seedURL)
	fmt.Printf("  📏 Max depth: %d levels\n", wc.maxDepth)
	fmt.Printf("  📊 Max pages: %d pages\n", wc.maxPages)
	fmt.Println()

	normalizedSeed := normalizeURL(seedURL)
	results := make(chan crawlBatch, 100)
	var wg sync.WaitGroup
	var resultsWg sync.WaitGroup

	// Start with the seed URL
	wg.Add(1)
	go wc.crawlPage(normalizedSeed, 0, results, &wg)

	// Process results and spawn new crawling tasks
	resultsWg.Add(1)
	go func() {
		defer resultsWg.Done()
		for batch := range results {
			for _, link := range batch.links {
				normLink := normalizeURL(link)
				wc.mutex.Lock()
				shouldCrawl := !wc.visited[normLink] && len(wc.visited) < wc.maxPages
				wc.mutex.Unlock()

				if shouldCrawl {
					wg.Add(1)
					go wc.crawlPage(normLink, batch.depth, results, &wg)
				}
			}
		}
	}()

	// Wait for all crawling to complete
	wg.Wait()
	close(results)
	resultsWg.Wait()

	fmt.Printf("\n  ✅ Crawling completed in %v\n", time.Since(wc.startTime))
	fmt.Printf("  📈 Discovered %d pages with %d total links\n", len(wc.graph), wc.getTotalLinks())
}

// getTotalLinks calculates total links in the graph
func (wc *WebCrawler) getTotalLinks() int {
	total := 0
	wc.mutex.Lock()
	for _, links := range wc.graph {
		total += len(links)
	}
	wc.mutex.Unlock()
	return total
}

// categorizeURL determines the category of a URL based on its path
func (wc *WebCrawler) categorizeURL(urlStr string) string {
	if u, err := url.Parse(urlStr); err == nil {
		path := strings.ToLower(u.Path)
		if path == "" || path == "/" {
			return "Homepage"
		} else if strings.Contains(path, "product") {
			return "Products"
		} else if strings.Contains(path, "about") || strings.Contains(path, "company") {
			return "About"
		} else if strings.Contains(path, "contact") {
			return "Contact"
		} else if strings.Contains(path, "blog") || strings.Contains(path, "news") {
			return "Content"
		} else if strings.Contains(path, "career") || strings.Contains(path, "job") {
			return "Careers"
		} else if strings.Contains(path, "api") {
			return "API/Tech"
		} else if strings.Contains(path, "industry") || strings.Contains(path, "solution") {
			return "Solutions"
		}
	}
	return "Other"
}

// CalculatePageRank implements the PageRank algorithm with enhanced metrics
func (wc *WebCrawler) CalculatePageRank(iterations int, dampingFactor float64) []PageRank {
	ranks := make(map[string]float64)
	newRanks := make(map[string]float64)
	inboundCount := make(map[string]int)

	// Get all URLs
	var urls []string
	for url := range wc.graph {
		urls = append(urls, url)
	}

	// Add URLs that are linked to but not crawled
	for _, links := range wc.graph {
		for _, link := range links {
			if !contains(urls, link) {
				urls = append(urls, link)
			}
		}
	}

	// Count inbound links
	for _, links := range wc.graph {
		for _, link := range links {
			inboundCount[link]++
		}
	}

	numPages := float64(len(urls))
	initialRank := 1.0 / numPages

	// Initialize all pages with equal rank
	for _, url := range urls {
		ranks[url] = initialRank
	}

	printSection("PAGERANK CALCULATION")
	fmt.Printf("  🧮 Processing %d pages through %d iterations\n", len(urls), iterations)
	fmt.Printf("  ⚖️  Damping factor: %.2f\n\n", dampingFactor)

	// Run PageRank iterations
	for iter := 0; iter < iterations; iter++ {
		// Initialize new ranks
		for _, url := range urls {
			newRanks[url] = (1.0 - dampingFactor) / numPages
		}

		// Calculate rank contributions, handling sinks
		for _, sourceURL := range urls {
			links, exists := wc.graph[sourceURL]
			if !exists {
				links = []string{}
			}
			if len(links) > 0 {
				contribution := dampingFactor * ranks[sourceURL] / float64(len(links))
				for _, targetURL := range links {
					newRanks[targetURL] += contribution
				}
			} else {
				// Sink node: distribute rank uniformly to all pages
				sinkContribution := dampingFactor * ranks[sourceURL] / numPages
				for _, targetURL := range urls {
					newRanks[targetURL] += sinkContribution
				}
			}
		}

		// Copy new ranks to current ranks
		for url, rank := range newRanks {
			ranks[url] = rank
		}

		if (iter+1)%5 == 0 || iter+1 == iterations {
			fmt.Printf("  ⏳ Progress: %d/%d iterations completed\n", iter+1, iterations)
		}
	}

	// Convert to PageRank slice with additional metrics
	var results []PageRank
	for url, rank := range ranks {
		outboundLinks := 0
		if links, exists := wc.graph[url]; exists {
			outboundLinks = len(links)
		}
		results = append(results, PageRank{
			URL:           url,
			Rank:          rank,
			InboundLinks:  inboundCount[url],
			OutboundLinks: outboundLinks,
			Category:      wc.categorizeURL(url),
		})
	}

	return results
}

// PrintDetailedResults displays comprehensive analysis
func (wc *WebCrawler) PrintDetailedResults(results []PageRank) {
	// Sort by rank (descending)
	sort.Slice(results, func(i, j int) bool {
		return results[i].Rank > results[j].Rank
	})

	printHeader("PAGERANK ANALYSIS REPORT")

	// Overall Statistics
	printSection("CRAWLING STATISTICS")
	stats := wc.generateStats(results)
	fmt.Printf("  📊 Pages Analyzed: %d\n", stats.TotalPages)
	fmt.Printf("  📄 Unique Pages: %d\n", stats.UniquePages)
	fmt.Printf("  🔗 Total Links Found: %d\n", stats.TotalLinks)
	fmt.Printf("  ❌ Dead Links: %d\n", stats.DeadLinks)
	fmt.Printf("  ⏱️  Crawl Duration: %v\n", stats.CrawlDuration)
	fmt.Printf("  📈 Average Links per Page: %.2f\n", stats.AvgLinksPerPage)

	// Top Pages by PageRank
	printSection("TOP 10 PAGES BY IMPORTANCE (PageRank)")
	fmt.Printf("%-8s %-12s %-8s %-8s %-12s %s\n", "RANK", "CATEGORY", "IN", "OUT", "SCORE", "URL")
	fmt.Println(strings.Repeat("─", 100))

	for i, result := range results {
		if i >= 10 {
			break
		}
		fmt.Printf("%-8d %-12s %-8d %-8d %-12.6f %s\n",
			i+1, result.Category, result.InboundLinks, result.OutboundLinks, result.Rank, result.URL)
	}

	// Category Analysis
	wc.printCategoryAnalysis(results)

	// Authority vs Hub Analysis
	wc.printAuthorityHubAnalysis(results)

	// Strategic Insights
	wc.printStrategicInsights(results)
}

// generateStats calculates crawling statistics
func (wc *WebCrawler) generateStats(results []PageRank) CrawlStats {
	totalLinks := wc.getTotalLinks()
	return CrawlStats{
		TotalPages:      len(results),
		TotalLinks:      totalLinks,
		CrawlDuration:   time.Since(wc.startTime),
		AvgLinksPerPage: float64(totalLinks) / float64(len(wc.graph)),
		DeadLinks:       wc.deadLinks,
		UniquePages:     len(wc.visited),
	}
}

// printCategoryAnalysis shows analysis by page categories
func (wc *WebCrawler) printCategoryAnalysis(results []PageRank) {
	printSection("ANALYSIS BY PAGE CATEGORY")

	categoryStats := make(map[string][]PageRank)
	for _, result := range results {
		categoryStats[result.Category] = append(categoryStats[result.Category], result)
	}

	fmt.Printf("%-15s %-8s %-12s %-12s\n", "CATEGORY", "COUNT", "AVG RANK", "TOP PAGE")
	fmt.Println(strings.Repeat("─", 65))

	for category, pages := range categoryStats {
		if len(pages) == 0 {
			continue
		}

		// Calculate average rank
		totalRank := 0.0
		maxRank := 0.0
		topURL := ""

		for _, page := range pages {
			totalRank += page.Rank
			if page.Rank > maxRank {
				maxRank = page.Rank
				topURL = page.URL
			}
		}
		avgRank := totalRank / float64(len(pages))

		// Truncate URL for display
		displayURL := topURL
		if len(displayURL) > 40 {
			displayURL = displayURL[:37] + "..."
		}

		fmt.Printf("%-15s %-8d %-12.6f %s\n", category, len(pages), avgRank, displayURL)
	}
}

// printAuthorityHubAnalysis shows authority vs hub analysis
func (wc *WebCrawler) printAuthorityHubAnalysis(results []PageRank) {
	printSection("AUTHORITY vs HUB ANALYSIS")

	// Sort by inbound links (authorities)
	authResults := make([]PageRank, len(results))
	copy(authResults, results)
	sort.Slice(authResults, func(i, j int) bool {
		return authResults[i].InboundLinks > authResults[j].InboundLinks
	})

	// Sort by outbound links (hubs)
	hubResults := make([]PageRank, len(results))
	copy(hubResults, results)
	sort.Slice(hubResults, func(i, j int) bool {
		return hubResults[i].OutboundLinks > hubResults[j].OutboundLinks
	})

	fmt.Printf("\n🏆 TOP AUTHORITIES (Most Inbound Links):\n")
	for i := 0; i < 5 && i < len(authResults); i++ {
		page := authResults[i]
		fmt.Printf("  %d. %s (%d inbound links)\n", i+1, page.URL, page.InboundLinks)
	}

	fmt.Printf("\n🌐 TOP HUBS (Most Outbound Links):\n")
	for i := 0; i < 5 && i < len(hubResults); i++ {
		page := hubResults[i]
		fmt.Printf("  %d. %s (%d outbound links)\n", i+1, page.URL, page.OutboundLinks)
	}
}

// printStrategicInsights provides business insights
func (wc *WebCrawler) printStrategicInsights(results []PageRank) {
	printSection("STRATEGIC INSIGHTS & RECOMMENDATIONS")

	// Find the homepage
	var homepage *PageRank
	for i := range results {
		u, _ := url.Parse(results[i].URL)
		if results[i].Category == "Homepage" && u.Path == "" {
			homepage = &results[i]
			break
		}
	}

	// Find highest ranking non-homepage
	var topContentPage *PageRank
	maxRank := 0.0
	for i := range results {
		if results[i].Category != "Homepage" && results[i].Rank > maxRank {
			maxRank = results[i].Rank
			topContentPage = &results[i]
		}
	}

	fmt.Println("📋 KEY FINDINGS:")
	if homepage != nil {
		fmt.Printf("   • Homepage PageRank: %.6f (links to %d pages)\n",
			homepage.Rank, homepage.OutboundLinks)
	} else {
		fmt.Println("   • Homepage not clearly identified in crawl results")
	}

	if topContentPage != nil {
		fmt.Printf("   • Most important content page: %s (%.6f)\n",
			topContentPage.Category, topContentPage.Rank)
	}

	// Calculate rank distribution
	totalRank := 0.0
	maxR := 0.0
	minR := 1.0
	for _, result := range results {
		totalRank += result.Rank
		if result.Rank > maxR {
			maxR = result.Rank
		}
		if result.Rank < minR {
			minR = result.Rank
		}
	}
	avgRank := totalRank / float64(len(results))

	fmt.Printf("   • Rank distribution: Max=%.6f, Min=%.6f, Avg=%.6f\n", maxR, minR, avgRank)

	// Count pages by category for insights
	categoryCount := make(map[string]int)
	highRankPages := 0
	for _, result := range results {
		categoryCount[result.Category]++
		if result.Rank > avgRank*1.1 {
			highRankPages++
		}
	}

	fmt.Printf("   • %d/%d pages (%.1f%%) have above-average PageRank\n",
		highRankPages, len(results), float64(highRankPages)/float64(len(results))*100)

	fmt.Println("\n💡 OPTIMIZATION RECOMMENDATIONS:")

	if categoryCount["Contact"] > 0 {
		fmt.Println("   • Consider adding more internal links to Contact pages for better conversion")
	}

	if categoryCount["Products"] > categoryCount["Solutions"] {
		fmt.Println("   • Product pages are well-represented - ensure they link to relevant solutions")
	}

	if homepage != nil && homepage.OutboundLinks > 20 {
		fmt.Printf("   • Homepage has %d links - consider grouping or prioritizing key pages\n", homepage.OutboundLinks)
	}

	if maxR/minR < 2.0 {
		fmt.Println("   • PageRank scores are very similar - consider improving internal link diversity")
	}

	fmt.Println("\n🎯 USE CASES FOR THIS ANALYSIS:")
	fmt.Println("   • SEO: Focus optimization efforts on high PageRank pages")
	fmt.Println("   • UX: Improve navigation to important but low-ranked pages")
	fmt.Println("   • Content: Create more content linking to strategic pages")
	fmt.Println("   • Business: Understand which solutions/products get most internal emphasis")
}

// getSeedDomain extracts domain from the first crawled URL
func (wc *WebCrawler) getSeedDomain() string {
	for urlStr := range wc.graph {
		parsed, err := url.Parse(urlStr)
		if err == nil {
			return parsed.Scheme + "://" + parsed.Host
		}
	}
	return ""
}

// CLI flag variables
var (
	seedURL       = flag.String("url", "", "Seed URL to start crawling (required)")
	maxDepth      = flag.Int("depth", 2, "Maximum crawl depth")
	maxPages      = flag.Int("pages", 50, "Maximum number of pages to crawl")
	iterations    = flag.Int("iterations", 20, "Number of PageRank iterations")
	dampingFactor = flag.Float64("damping", 0.85, "PageRank damping factor")
	timeout       = flag.Duration("timeout", 15*time.Second, "HTTP client timeout")
	verbose       = flag.Bool("verbose", false, "Enable verbose output")
)

func printUsage() {
	fmt.Println("🕷️  Web Crawler & PageRank Calculator")
	fmt.Println("=====================================")
	fmt.Println()
	fmt.Println("USAGE:")
	fmt.Println("  webcrawler -url=<website> [options]")
	fmt.Println()
	fmt.Println("REQUIRED:")
	fmt.Println("  -url string")
	fmt.Println("        Seed URL to start crawling (e.g., https://example.com)")
	fmt.Println()
	fmt.Println("OPTIONS:")
	fmt.Println("  -depth int")
	fmt.Println("        Maximum crawl depth (default 2)")
	fmt.Println("  -pages int")
	fmt.Println("        Maximum number of pages to crawl (default 50)")
	fmt.Println("  -iterations int")
	fmt.Println("        Number of PageRank iterations (default 20)")
	fmt.Println("  -damping float")
	fmt.Println("        PageRank damping factor (default 0.85)")
	fmt.Println("  -timeout duration")
	fmt.Println("        HTTP client timeout (default 15s)")
	fmt.Println("  -verbose")
	fmt.Println("        Enable verbose output")
	fmt.Println()
	fmt.Println("EXAMPLES:")
	fmt.Println("  webcrawler -url=https://example.com")
	fmt.Println("  webcrawler -url=https://mybusiness.com -depth=3 -pages=100")
	fmt.Println("  webcrawler -url=https://blog.com -iterations=30 -verbose")
	fmt.Println()
}

func main() {
	flag.Usage = printUsage
	flag.Parse()

	// Validate required arguments
	if *seedURL == "" {
		printUsage()
		os.Exit(1)
	}

	// Validate URL format
	if _, err := url.Parse(*seedURL); err != nil {
		fmt.Printf("❌ Error: Invalid URL format: %s\n", *seedURL)
		os.Exit(1)
	}

	// Display configuration
	printHeader("WEB CRAWLER & PAGERANK CALCULATOR")
	fmt.Printf("🎯 Target: %s\n", *seedURL)
	fmt.Printf("📏 Max Depth: %d levels\n", *maxDepth)
	fmt.Printf("📄 Max Pages: %d pages\n", *maxPages)
	fmt.Printf("🔄 PageRank Iterations: %d\n", *iterations)
	fmt.Printf("⚖️  Damping Factor: %.2f\n", *dampingFactor)
	fmt.Printf("⏱️  Timeout: %v\n", *timeout)
	if *verbose {
		fmt.Printf("📝 Verbose Mode: Enabled\n")
	}

	// Create and run crawler
	crawler := NewWebCrawler(*maxDepth, *maxPages, *timeout, *verbose)

	// Perform crawling
	crawler.Crawl(*seedURL)

	// Calculate PageRank
	results := crawler.CalculatePageRank(*iterations, *dampingFactor)

	// Display comprehensive results
	crawler.PrintDetailedResults(results)

	fmt.Printf("\n🎉 Analysis completed in %v\n", time.Since(crawler.startTime))
	fmt.Println("📊 Use these insights to optimize your website's internal linking strategy!")
}
