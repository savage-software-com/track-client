package track

import (
	"bytes"
	"compress/gzip"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// maxHead is how much of a page is read to find its title.
	maxHead = 16 << 10
	// maxPath is the longest path that is counted; longer ones are not
	// pages.
	maxPath = 500
	// maxText bounds the title, the campaign fields and event names.
	maxText = 300
)

// Middleware counts the pages of next that visitors open. The response is
// never delayed: the view is noted after the page was written.
func (t *Tracker) Middleware(next http.Handler) http.Handler {
	if t.disabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !countable(r) || t.config.Skip != nil && t.config.Skip(r) {
			next.ServeHTTP(w, r)

			return
		}

		recorder := &recorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}

		if status != http.StatusOK && status != http.StatusNotFound {
			return
		}

		if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			return
		}

		if status == http.StatusNotFound {
			t.add(t.event(r, NotFound))

			return
		}

		// A site that compresses its pages itself, below this middleware,
		// writes them compressed.
		head := plainHead(w.Header(), recorder.head)

		if private(w.Header()) || noindexMeta(head) {
			return
		}

		event := t.event(r, Pageview)
		event.Title = withoutSiteName(title(head), t.config.SiteName)
		t.add(event)
	})
}

// Event counts an event of the site's own, such as "signup" or "download",
// for the visitor of r. The dashboard lists the events by name, with how
// often they happened and for how many visitors.
func (t *Tracker) Event(r *http.Request, name string) {
	name = truncate(strings.Join(strings.Fields(name), " "), maxText)
	if t.disabled || name == "" || isBot(r.UserAgent()) {
		return
	}

	event := t.event(r, Custom)
	event.Name = name
	t.add(event)
}

// event is what the tracker sends for r.
func (t *Tracker) event(r *http.Request, kind Kind) Event {
	query := r.URL.Query()

	source := query.Get("utm_source")
	if source == "" {
		source = query.Get("ref")
	}

	return Event{
		Kind:      kind,
		Time:      time.Now().UTC(),
		Host:      t.host(r),
		Path:      r.URL.Path,
		Referrer:  referrer(r.Referer(), t.host(r)),
		Source:    field(source),
		Medium:    field(query.Get("utm_medium")),
		Campaign:  field(query.Get("utm_campaign")),
		Language:  language(r.Header.Get("Accept-Language")),
		UserAgent: r.UserAgent(),
		IP:        t.clientIP(r),
	}
}

// host is the site's host name: Config.Hostname, or the request's host
// without its port.
func (t *Tracker) host(r *http.Request) string {
	if t.config.Hostname != "" {
		return strings.ToLower(t.config.Hostname)
	}

	return strings.ToLower(hostname(r.Host))
}

// clientIP is the visitor's address: Config.ClientIP, else the direct
// peer, or with trusted proxies the first address from the right of the
// forwarding headers that is not one of them.
func (t *Tracker) clientIP(r *http.Request) string {
	if t.config.ClientIP != nil {
		return t.config.ClientIP(r)
	}

	peer := hostname(r.RemoteAddr)

	addr, err := netip.ParseAddr(peer)
	if err != nil || !t.trusted(addr) {
		return peer
	}

	forwarded := forwardedFor(r.Header)
	for i := len(forwarded) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(forwarded[i])
		if err != nil {
			return peer
		}

		if !t.trusted(hop) {
			return hop.Unmap().String()
		}
	}

	return peer
}

func (t *Tracker) trusted(addr netip.Addr) bool {
	addr = addr.Unmap()

	for _, prefix := range t.proxies {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}

// forwardedFor lists the addresses a proxy reports, the visitor first:
// X-Forwarded-For, else X-Real-IP, else the for= of Forwarded.
func forwardedFor(header http.Header) []string {
	var hops []string

	for _, value := range header.Values("X-Forwarded-For") {
		for hop := range strings.SplitSeq(value, ",") {
			hops = append(hops, hostname(strings.TrimSpace(hop)))
		}
	}

	if len(hops) > 0 {
		return hops
	}

	if real := strings.TrimSpace(header.Get("X-Real-IP")); real != "" {
		return []string{hostname(real)}
	}

	for _, value := range header.Values("Forwarded") {
		for element := range strings.SplitSeq(value, ",") {
			for pair := range strings.SplitSeq(element, ";") {
				key, value, _ := strings.Cut(strings.TrimSpace(pair), "=")
				if strings.EqualFold(key, "for") {
					hops = append(hops, hostname(strings.Trim(value, `"`)))
				}
			}
		}
	}

	return hops
}

// countable reports whether r is a person opening a page: not a file, not a
// form post, not a prefetch and not a known bot.
func countable(r *http.Request) bool {
	if r.Method != http.MethodGet || len(r.URL.Path) > maxPath || !strings.HasPrefix(r.URL.Path, "/") {
		return false
	}

	if path.Ext(r.URL.Path) != "" {
		return false
	}

	// Browsers announce speculative loads; the visitor may never see them.
	for _, header := range []string{"Sec-Purpose", "Purpose", "X-Purpose", "X-Moz"} {
		if strings.Contains(strings.ToLower(r.Header.Get(header)), "prefetch") {
			return false
		}
	}

	// htmx fragments and Turbo frames are parts of a page that was already
	// counted; boosted links are real navigation.
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		return false
	}

	if r.Header.Get("Turbo-Frame") != "" {
		return false
	}

	return !isBot(r.UserAgent())
}

// private reports whether a response asks not to be kept: search engines
// may not index it (X-Robots-Tag: noindex or none, also for one crawler as
// in "googlebot: noindex") or only the visitor's own browser may store it
// (Cache-Control: private). Such pages show personal details or are endless
// variations, such as search results; their addresses must not end up in
// statistics. A bare "no-store" still counts: forms send it to stay fresh.
func private(header http.Header) bool {
	for _, value := range header.Values("X-Robots-Tag") {
		if hasDirective(value, "noindex", "none") {
			return true
		}
	}

	for _, value := range header.Values("Cache-Control") {
		if hasDirective(value, "private") {
			return true
		}
	}

	return false
}

// metaTag and metaAttribute find <meta name="robots" content="…"> in a page.
var (
	metaTag       = regexp.MustCompile(`(?is)<meta\s[^>]*>`)
	metaAttribute = regexp.MustCompile(`(?is)\b(name|content)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// noindexMeta reports whether the page's head asks robots not to index it.
func noindexMeta(head []byte) bool {
	for _, tag := range metaTag.FindAll(head, -1) {
		var name, content string

		for _, attribute := range metaAttribute.FindAllSubmatch(tag, -1) {
			value := string(attribute[2]) + string(attribute[3]) + string(attribute[4])

			if strings.EqualFold(string(attribute[1]), "name") {
				name = value
			} else {
				content = value
			}
		}

		if strings.EqualFold(strings.TrimSpace(name), "robots") && hasDirective(content, "noindex", "none") {
			return true
		}
	}

	return false
}

// hasDirective reports whether a header or meta value, a list such as
// "private, max-age=0" or "googlebot: noindex, nofollow", holds one of
// directives.
func hasDirective(value string, directives ...string) bool {
	separator := func(r rune) bool {
		return r == ',' || r == ':' || r == ' ' || r == '\t'
	}

	for field := range strings.FieldsFuncSeq(strings.ToLower(value), separator) {
		if slices.Contains(directives, field) {
			return true
		}
	}

	return false
}

// botMarkers are parts of the user agents of crawlers, link previews,
// monitors and scripts.
var botMarkers = []string{
	"bot", "crawl", "spider", "slurp", "preview", "facebookexternalhit", "whatsapp", "embedly", "headless",
	"phantomjs", "lighthouse", "pagespeed", "uptime", "monitor", "statuscake", "pingdom", "datadog", "newrelic",
	"curl", "wget", "python", "go-http-client", "httpclient", "okhttp", "java/", "libwww", "scrapy", "axios",
	"node-fetch", "feedfetcher", "validator", "chrome-lighthouse", "gtmetrix", "dataprovider", "httrack",
}

// isBot reports user agents that are not a person's browser. Without a
// user agent it is certainly not a browser.
func isBot(agent string) bool {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" || !strings.HasPrefix(agent, "mozilla/") && !strings.HasPrefix(agent, "opera/") {
		return true
	}

	for _, marker := range botMarkers {
		if strings.Contains(agent, marker) {
			return true
		}
	}

	return false
}

// referrer is the host of the site the visitor came from, without "www.",
// or "" for direct visits and links within the site.
func referrer(value, own string) string {
	from, err := url.Parse(value)
	if err != nil || from.Scheme != "http" && from.Scheme != "https" {
		return ""
	}

	host := strings.TrimPrefix(strings.ToLower(from.Hostname()), "www.")
	if host == "" || len(host) > 253 || host == strings.TrimPrefix(own, "www.") {
		return ""
	}

	return host
}

// language is the primary subtag of the first language the browser asks
// for, such as "nl" for "nl-NL,nl;q=0.9,en;q=0.8".
func language(accept string) string {
	first, _, _ := strings.Cut(accept, ",")
	first, _, _ = strings.Cut(first, ";")
	primary, _, _ := strings.Cut(strings.TrimSpace(first), "-")
	primary = strings.ToLower(primary)

	if len(primary) < 2 || len(primary) > 3 || strings.Trim(primary, "abcdefghijklmnopqrstuvwxyz") != "" {
		return ""
	}

	return primary
}

// field is a campaign value as it is counted: one line, at most 100 bytes.
func field(value string) string {
	return truncate(strings.Join(strings.Fields(value), " "), 100)
}

func hostname(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}

	return strings.Trim(host, "[]")
}

// separators join a page title and the site name in a layout's <title>.
var separators = []string{" · ", " | ", " - ", " – ", " — ", " :: ", " / "}

// withoutSiteName removes the site name that a layout adds before or after
// the page title, as in "About · My site" or "My site | About". A title
// that is only the site name, as on many home pages, is kept.
func withoutSiteName(title, site string) string {
	if site == "" || title == site {
		return title
	}

	for _, separator := range separators {
		if page, ok := strings.CutSuffix(title, separator+site); ok && strings.TrimSpace(page) != "" {
			return strings.TrimSpace(page)
		}

		if page, ok := strings.CutPrefix(title, site+separator); ok && strings.TrimSpace(page) != "" {
			return strings.TrimSpace(page)
		}
	}

	return title
}

// title is the text of the page's <title> element, or "".
func title(head []byte) string {
	lower := bytes.ToLower(head)

	open := bytes.Index(lower, []byte("<title"))
	if open < 0 {
		return ""
	}

	start := bytes.IndexByte(lower[open:], '>')
	if start < 0 {
		return ""
	}

	start += open + 1

	end := bytes.Index(lower[start:], []byte("</title"))
	if end < 0 {
		return ""
	}

	text := strings.Join(strings.Fields(html.UnescapeString(string(head[start:start+end]))), " ")

	return truncate(text, maxText)
}

// truncate shortens s to at most limit bytes without splitting a
// character.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}

	return s
}

// gzipReaders are readers of compressed pages, kept for the next page.
var gzipReaders sync.Pool

// plainHead is the start of a page as the recorder kept it, decompressed
// when the response is gzip-compressed. A compressed head is cut off, so it
// decompresses into what it holds.
func plainHead(header http.Header, head []byte) []byte {
	if !strings.EqualFold(header.Get("Content-Encoding"), "gzip") {
		return head
	}

	reader, _ := gzipReaders.Get().(*gzip.Reader)

	var err error

	if reader == nil {
		reader, err = gzip.NewReader(bytes.NewReader(head))
	} else {
		err = reader.Reset(bytes.NewReader(head))
	}

	if err != nil {
		return nil
	}

	defer gzipReaders.Put(reader)

	plain := make([]byte, maxHead)

	// The head ends before the compressed page does.
	n, _ := io.ReadFull(reader, plain)

	return plain[:n]
}

// recorder notes the status of a response and keeps its first bytes, which
// hold the page's title.
type recorder struct {
	http.ResponseWriter

	status int
	head   []byte
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}

	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}

	if r.status == http.StatusOK && len(r.head) < maxHead {
		r.head = append(r.head, data[:min(len(data), maxHead-len(r.head))]...)
	}

	return r.ResponseWriter.Write(data)
}

// Flush keeps streaming responses working through the recorder.
func (r *recorder) Flush() {
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the original writer.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
