package track

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const browser = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15"

// collector is a fake Track that keeps what it receives.
type collector struct {
	mu     sync.Mutex
	events []Event
	tokens []string
	status int
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.tokens = append(c.tokens, r.Header.Get("Authorization"))

	if c.status != 0 {
		w.WriteHeader(c.status)

		return
	}

	var batch Batch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	c.events = append(c.events, batch.Events...)
	w.WriteHeader(http.StatusAccepted)
}

func (c *collector) received() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]Event(nil), c.events...)
}

func startTracker(t *testing.T, endpoint string, configure ...func(*Config)) *Tracker {
	t.Helper()

	config := Config{Token: "trk_test", Endpoint: endpoint, FlushInterval: time.Hour}
	for _, fn := range configure {
		fn(&config)
	}

	tracker, err := New(config)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = tracker.Close(context.Background()) })

	return tracker
}

func page(status int, body string, headers ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}

		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func visit(handler http.Handler, method, target string, headers ...string) {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("User-Agent", browser)

	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestMiddlewareCountsPages(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL, func(c *Config) { c.SiteName = "Example" })
	site := tracker.Middleware(page(200, "<html><head><title>About &amp; us · Example</title></head><body>hi</body></html>"))

	visit(site, http.MethodGet, "http://example.com/about?utm_source=news&utm_campaign=Autumn+sale&email=a@b.c",
		"Referer", "https://www.google.com/search?q=x",
		"Accept-Language", "nl-NL,nl;q=0.9,en;q=0.8",
	)

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := fake.received()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}

	got := events[0]
	if got.Duration <= 0 {
		t.Errorf("duration = %d µs, want the time the page took", got.Duration)
	}

	got.Duration = 0
	want := Event{
		Kind: Pageview, Host: "example.com", Path: "/about", Title: "About & us", Referrer: "google.com",
		Source: "news", Campaign: "Autumn sale", Language: "nl", UserAgent: browser, IP: "192.0.2.0",
	}

	got.Time = time.Time{}
	if got != want {
		t.Errorf("event = %+v\nwant    %+v", got, want)
	}

	if fake.tokens[0] != "Bearer trk_test" {
		t.Errorf("Authorization = %q", fake.tokens[0])
	}
}

func TestMiddlewareSkips(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL, func(c *Config) {
		c.Skip = func(r *http.Request) bool { return r.URL.Query().Has("editor") }
	})

	html := tracker.Middleware(page(200, "<title>Page</title>"))

	visit(html, http.MethodPost, "/form")
	visit(html, http.MethodGet, "/style.css")
	visit(html, http.MethodGet, "/page", "Sec-Purpose", "prefetch")
	visit(html, http.MethodGet, "/page", "HX-Request", "true")
	visit(html, http.MethodGet, "/page", "User-Agent", "Googlebot/2.1 (+http://www.google.com/bot.html)")
	visit(html, http.MethodGet, "/page", "User-Agent", "curl/8.4.0")
	visit(html, http.MethodGet, "/page?editor")
	visit(tracker.Middleware(page(200, "<title>Search</title>", "X-Robots-Tag", "noindex")), http.MethodGet, "/search")
	visit(tracker.Middleware(page(200, `<head><meta name="robots" content="noindex,follow"></head>`)), http.MethodGet, "/me")
	visit(tracker.Middleware(page(200, "<title>Account</title>", "Cache-Control", "private, no-store")), http.MethodGet, "/account")
	visit(tracker.Middleware(page(500, "<title>Error</title>")), http.MethodGet, "/broken")

	// Boosted links are navigation.
	visit(html, http.MethodGet, "/boosted", "HX-Request", "true", "HX-Boosted", "true")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Bots come as bots, without an address; the 500 as a server error.
	var got []string

	for _, event := range fake.received() {
		got = append(got, string(event.Kind)+" "+event.Path)

		if event.Kind == Bot && (event.IP != "" || event.Referrer != "") {
			t.Errorf("a bot's request carries more than its user agent: %+v", event)
		}
	}

	// The worker and Flush make events side by side, in no fixed order.
	want := []string{"bot /page", "bot /page", "error /broken", "pageview /boosted"}
	slices.Sort(got)

	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("events = %v\nwant     %v", got, want)
	}
}

func TestDownloads(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)
	file := tracker.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")

		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
		}

		_, _ = io.WriteString(w, "%PDF-1.7")
	}))

	visit(file, http.MethodGet, "/files/Prices.PDF")
	visit(file, http.MethodGet, "/files/manual.pdf", "Range", "bytes=0-65535")
	visit(file, http.MethodGet, "/files/manual.pdf", "Range", "bytes=65536-131071")
	visit(file, http.MethodGet, "/style.css")
	visit(file, http.MethodHead, "/files/prices.pdf")
	visit(file, http.MethodGet, "/files/prices.pdf", "User-Agent", "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, event := range fake.received() {
		got = append(got, string(event.Kind)+" "+event.Path)
	}

	want := []string{"bot /files/prices.pdf", "download /files/Prices.PDF", "download /files/manual.pdf"}
	slices.Sort(got)

	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("events = %v\nwant     %v", got, want)
	}

	// A feed reader counts as a bot, also for a feed with an extension.
	req := httptest.NewRequest(http.MethodGet, "/feed.xml", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Inoreader/1.0; +http://www.inoreader.com/; 45 subscribers; )")

	if tracker.classify(req) != botRequest {
		t.Error("a feed reader's fetch of /feed.xml is not counted")
	}

	// An empty list counts no downloads.
	none := startTracker(t, server.URL, func(c *Config) { c.Downloads = []string{} })
	if none.classify(httptest.NewRequest(http.MethodGet, "/files/prices.pdf", nil)) != ignored {
		t.Error("a download counted with Downloads empty")
	}
}

func TestMiddlewareCountsNotFound(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)
	visit(tracker.Middleware(page(404, "<title>Not found</title>")), http.MethodGet, "/old-page")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := fake.received()
	if len(events) != 1 || events[0].Kind != NotFound || events[0].Path != "/old-page" {
		t.Fatalf("events = %+v", events)
	}
}

func TestSecurityCheck(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)

	site := httptest.NewServer(tracker.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.env" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>A page</title>")
	})))
	defer site.Close()

	key := TokenKey("trk_test")

	get := func(path, signature string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, site.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("User-Agent", browser)

		if signature != "" {
			req.Header.Set(CheckHeader, signature)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)

		return resp, string(body)
	}

	// A real signature gets proof, and its requests are not counted.
	resp, body := get("/", SignCheck(key, time.Now(), "n1"))
	if resp.Header.Get(CheckProofHeader) != CheckProof(key, "n1") || !strings.Contains(body, "A page") {
		t.Errorf("proof %q, body %q", resp.Header.Get(CheckProofHeader), body)
	}

	if resp, _ := get("/.env", SignCheck(key, time.Now(), "n2")); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/.env: %d", resp.StatusCode)
	}

	resp, body = get(CheckPath, SignCheck(key, time.Now(), "n3"))

	var report CheckReport
	if err := json.Unmarshal([]byte(body), &report); err != nil || report.Go == "" || resp.Header.Get(CheckProofHeader) == "" {
		t.Errorf("report %q: %v", body, err)
	}

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 0 {
		t.Errorf("a signed request was counted: %+v", events)
	}

	// Anything else is an ordinary request: no proof, no report, and counted,
	// so a scanner cannot hide behind a made-up signature.
	for _, signature := range []string{
		"", "nonsense", "1.2.3",
		SignCheck(TokenKey("trk_other"), time.Now(), "n4"),
		SignCheck(key, time.Now().Add(-time.Hour), "n5"),
		SignCheck(key, time.Now().Add(time.Hour), "n6"),
	} {
		resp, body := get("/.env", signature)
		if resp.Header.Get(CheckProofHeader) != "" || resp.StatusCode != http.StatusNotFound || strings.Contains(body, "modules") {
			t.Errorf("signature %q: proof %q, status %d", signature, resp.Header.Get(CheckProofHeader), resp.StatusCode)
		}
	}

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 6 {
		t.Errorf("%d events, want the 6 ordinary requests", len(events))
	}
}

func TestProbeKind(t *testing.T) {
	tests := map[string]string{
		"/.git/config":                    ProbeSource,
		"/server/.git/HEAD":               ProbeSource,
		"/.env":                           ProbeSecrets,
		"/.env.production":                ProbeSecrets,
		"/app/.ENV":                       ProbeSecrets,
		"/.aws/credentials":               ProbeSecrets,
		"/latest/meta-data/iam":           ProbeSecrets,
		"/server.key":                     ProbeSecrets,
		"/wp-login.php":                   ProbeWordPress,
		"/wp-includes/js/":                ProbeWordPress,
		"/cache-wordpress/":               ProbeWordPress,
		"/xmlrpc.php":                     ProbeWordPress,
		"/phpmyadmin/":                    ProbeTools,
		"/_profiler/phpinfo":              ProbeTools,
		"/webroot/index.php/_environment": ProbeTools,
		"/shell.php":                      ProbeScripts,
		"/backup/":                        ProbeBackup,
		"/backup-backup/includes/":        ProbeBackup,
		"/dump.sql":                       ProbeBackup,
		"/":                               "",
		"/pricing":                        "",
		"/about/wp":                       "",
		"/blog/environment":               "",
		"/assets/app.js":                  "",
		"/sitemap.xml":                    "",
		"/.well-known/assetlinks.json":    "",
	}

	for address, want := range tests {
		if got := ProbeKind(address); got != want {
			t.Errorf("ProbeKind(%q) = %q, want %q", address, got, want)
		}
	}
}

func TestMiddlewareCountsScanAttemptsWithoutAnyoneInThem(t *testing.T) {
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	file := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "SECRET=1")
	})
	pageWithTitle := page(200, "<title>Our WordPress tips</title>")
	redirect := http.RedirectHandler("/elsewhere", http.StatusMovedPermanently)

	tests := []struct {
		name    string
		handler http.Handler
		method  string
		path    string
		headers []string
		kind    Kind
		status  int
	}{
		{"a browser", notFound, http.MethodGet, "/.git/config", nil, Probe, 404},
		{"with a file extension", notFound, http.MethodGet, "/.env", nil, Probe, 404},
		{"a post", notFound, http.MethodPost, "/xmlrpc.php", nil, Probe, 404},
		{"a script", notFound, http.MethodGet, "/.env", []string{"User-Agent", "curl/8.0"}, Probe, 404},
		{"sending Global Privacy Control", notFound, http.MethodGet, "/.env", []string{"Sec-GPC", "1"}, Probe, 404},
		{"sending Do Not Track", notFound, http.MethodGet, "/wp-login.php", []string{"DNT", "1"}, Probe, 404},
		{"with the opt-out cookie", notFound, http.MethodGet, "/.env", []string{"Cookie", OptOutCookie + "=1"}, Probe, 404},
		{"a file that is really there", file, http.MethodGet, "/.env", nil, Probe, 200},
		{"a page that is really there", pageWithTitle, http.MethodGet, "/wordpress-tips", nil, Pageview, 0},
		{"that page, for someone who objected", pageWithTitle, http.MethodGet, "/wordpress-tips", []string{"Sec-GPC", "1"}, "", 0},
		{"a redirect", redirect, http.MethodGet, "/wp-admin", nil, "", 0},
		{"a bot that objects is still a bot", pageWithTitle, http.MethodGet, "/pricing", []string{"User-Agent", "curl/8.0", "Sec-GPC", "1"}, Bot, 200},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &collector{}
			server := httptest.NewServer(fake)
			defer server.Close()

			tracker := startTracker(t, server.URL)

			site := httptest.NewServer(tracker.Middleware(test.handler))
			defer site.Close()

			req, err := http.NewRequest(test.method, site.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("User-Agent", browser)
			req.Header.Set("Referer", "https://example.org/")

			for i := 0; i+1 < len(test.headers); i += 2 {
				req.Header.Set(test.headers[i], test.headers[i+1])
			}

			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}

			_ = resp.Body.Close()

			if err := tracker.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}

			events := fake.received()
			if test.kind == "" {
				if len(events) != 0 {
					t.Fatalf("counted: %+v", events)
				}

				return
			}

			if len(events) != 1 || events[0].Kind != test.kind {
				t.Fatalf("events = %+v, want one %s", events, test.kind)
			}

			if test.status != 0 && events[0].Status != test.status {
				t.Errorf("status = %d, want %d", events[0].Status, test.status)
			}

			if test.kind == Probe {
				event := events[0]
				if event.IP != "" || event.UserAgent != "" || event.Referrer != "" || event.Language != "" || event.From != "" {
					t.Errorf("a scan attempt says something about who asked: %+v", event)
				}
			}
		})
	}
}

func TestOptOut(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL, func(c *Config) { c.OptOutDomain = "example.com" })

	site := httptest.NewServer(tracker.Middleware(page(200, "<title>A page</title>")))
	defer site.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	get := func(path string, headers ...string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, site.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("User-Agent", browser)
		req.Header.Set("Accept-Language", "nl-NL,nl;q=0.9")

		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)

		return resp, string(body)
	}

	// The page is in the visitor's language, and is not counted itself.
	resp, body := get(DefaultOptOutPath)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Tel mijn bezoeken niet mee") || !strings.Contains(body, `value="out"`) {
		t.Fatalf("page: %d %s", resp.StatusCode, body)
	}

	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("looking at the page set a cookie")
	}

	form := url.Values{"choice": {"out"}}

	post, err := client.PostForm(site.URL+DefaultOptOutPath, form)
	if err != nil {
		t.Fatal(err)
	}
	_ = post.Body.Close()

	if post.StatusCode != http.StatusSeeOther || len(post.Cookies()) != 1 {
		t.Fatalf("post: %d %v", post.StatusCode, post.Cookies())
	}

	cookie := post.Cookies()[0]
	if cookie.Name != OptOutCookie || cookie.Value != "1" || cookie.MaxAge <= 0 || !cookie.HttpOnly || cookie.Domain != "example.com" {
		t.Errorf("cookie = %+v", cookie)
	}

	// Who objected, by cookie or by the browser's signal, is not counted.
	chosen := OptOutCookie + "=1"

	_, body = get("/page", "Cookie", chosen)
	if !strings.Contains(body, "A page") {
		t.Errorf("the page did not reach a visitor who objected: %s", body)
	}

	get("/page", "Sec-GPC", "1")
	get("/page", "DNT", "1")

	_, body = get(DefaultOptOutPath, "Cookie", chosen)
	if !strings.Contains(body, "Tel mijn bezoeken weer mee") {
		t.Errorf("no way back: %s", body)
	}

	_, body = get(DefaultOptOutPath, "Sec-GPC", "1")
	if !strings.Contains(body, "Global Privacy Control") || strings.Contains(body, "<form") {
		t.Errorf("the browser's signal: %s", body)
	}

	req := httptest.NewRequest(http.MethodPost, "/signup", nil)
	req.Header.Set("User-Agent", browser)
	req.Header.Set("Sec-GPC", "1")
	tracker.Event(req, "sign up")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 0 {
		t.Errorf("counted: %+v", events)
	}

	// Everyone else still is, and going back clears the cookie.
	get("/page")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 1 {
		t.Errorf("events = %+v", events)
	}

	post, err = client.PostForm(site.URL+DefaultOptOutPath, url.Values{"choice": {"in"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = post.Body.Close()

	if cookies := post.Cookies(); len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Errorf("going back: %v", cookies)
	}
}

func TestMaskIP(t *testing.T) {
	tests := map[string]string{
		"203.0.113.9":             "203.0.113.0",
		"::ffff:203.0.113.9":      "203.0.113.0",
		"2001:db8:1234:5678::1":   "2001:db8:1234::",
		"2001:db8:abcd:1:2:3:4:5": "2001:db8:abcd::",
		"fe80::1%eth0":            "fe80::",
		"":                        "",
		"not an address":          "",
	}

	for ip, want := range tests {
		if got := MaskIP(ip); got != want {
			t.Errorf("MaskIP(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestMiddlewareSendsNoFullAddressAndThePreviousPage(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL, func(c *Config) {
		c.ClientIP = func(*http.Request) string { return "203.0.113.9" }
	})
	handler := tracker.Middleware(page(200, "<title>A page</title>"))

	tests := []struct{ referer, from, referrer string }{
		{"https://example.com/pricing?coupon=abc#top", "/pricing", ""},
		{"https://www.example.com/", "/", ""},
		{"https://example.com", "/", ""},
		{"https://other.example/pricing", "", "other.example"},
		{"android-app://com.example/", "", ""},
		{"", "", ""},
	}

	for _, test := range tests {
		visit(handler, http.MethodGet, "https://example.com/next", "Referer", test.referer)
	}

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := fake.received()
	if len(events) != len(tests) {
		t.Fatalf("%d events, want %d", len(events), len(tests))
	}

	for i, test := range tests {
		event := events[i]
		if event.From != test.from || event.Referrer != test.referrer || event.IP != "203.0.113.0" {
			t.Errorf("Referer %q: from %q, referrer %q, ip %q", test.referer, event.From, event.Referrer, event.IP)
		}
	}
}

func TestMiddlewareGoesByTheTypeTheVisitorReceives(t *testing.T) {
	plainNotFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	write := func(status int, headers map[string]string, parts ...string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for name, value := range headers {
				w.Header().Set(name, value)
			}

			if status != http.StatusOK {
				w.WriteHeader(status)
			}

			for _, part := range parts {
				_, _ = io.WriteString(w, part)
			}
		})
	}

	json := map[string]string{"Content-Type": "application/json"}
	const html = "<!doctype html><html><head><title>Hello</title></head></html>"

	tests := []struct {
		name    string
		handler http.Handler
		kind    Kind
		title   string
	}{
		{"a page that names no type", write(http.StatusOK, nil, html), Pageview, "Hello"},
		{"a page written in small parts", write(http.StatusOK, nil, "<!doc", "type html><html><head>", "<title>Hello</title>"), Pageview, "Hello"},
		{"plain text that names no type", write(http.StatusOK, nil, "just some text"), "", ""},
		{"JSON", write(http.StatusOK, json, `{"a":1}`), "", ""},
		{"compressed without a type", write(http.StatusOK, map[string]string{"Content-Encoding": "gzip"}, html), "", ""},
		{"nothing at all", write(http.StatusOK, nil), "", ""},
		{"net/http's own 404", plainNotFound, NotFound, ""},
		{"a 404 page that names no type", write(http.StatusNotFound, nil, html), NotFound, ""},
		{"a plain 404 that names no type", write(http.StatusNotFound, nil, "not here"), NotFound, ""},
		{"the 404 of an API", write(http.StatusNotFound, json, `{"error":"gone"}`), "", ""},
		{"an empty 404", write(http.StatusNotFound, nil), "", ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &collector{}
			server := httptest.NewServer(fake)
			defer server.Close()

			tracker := startTracker(t, server.URL)

			// A real server: httptest's recorder fills in the type itself.
			site := httptest.NewServer(tracker.Middleware(test.handler))
			defer site.Close()

			req, err := http.NewRequest(http.MethodGet, site.URL+"/some-page", nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("User-Agent", browser)
			req.Header.Set("Accept-Encoding", "identity")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}

			_ = resp.Body.Close()

			if err := tracker.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}

			events := fake.received()

			if test.kind == "" {
				if len(events) != 0 {
					t.Fatalf("counted: %+v", events)
				}

				return
			}

			if len(events) != 1 || events[0].Kind != test.kind || events[0].Title != test.title {
				t.Fatalf("events = %+v, want one %s titled %q", events, test.kind, test.title)
			}
		})
	}
}

func TestMiddlewareKeepsWhatTheVisitorReceives(t *testing.T) {
	const html = "<!doctype html><title>Hello</title>"

	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)

	site := httptest.NewServer(tracker.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, html)
	})))
	defer site.Close()

	resp, err := http.Get(site.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != html || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("got %q as %q", body, resp.Header.Get("Content-Type"))
	}
}

func TestCustomEvent(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)

	req := httptest.NewRequest(http.MethodPost, "/signup", nil)
	req.Header.Set("User-Agent", browser)
	tracker.Event(req, "  sign   up ")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := fake.received()
	if len(events) != 1 || events[0].Kind != Custom || events[0].Name != "sign up" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRetriesWhenTrackIsDown(t *testing.T) {
	fake := &collector{status: http.StatusServiceUnavailable}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)
	site := tracker.Middleware(page(200, "<title>Home</title>"))
	visit(site, http.MethodGet, "/")

	if err := tracker.Flush(context.Background()); err == nil {
		t.Fatal("Flush succeeded while Track is down")
	}

	fake.mu.Lock()
	fake.status = 0
	fake.mu.Unlock()

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 1 {
		t.Fatalf("events = %d after retry, want 1", len(events))
	}

	if tracker.Dropped() != 0 {
		t.Errorf("dropped = %d", tracker.Dropped())
	}
}

func TestRefusedEventsAreDropped(t *testing.T) {
	fake := &collector{status: http.StatusUnauthorized}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)
	visit(tracker.Middleware(page(200, "<title>Home</title>")), http.MethodGet, "/")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if tracker.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", tracker.Dropped())
	}
}

func TestCloseSendsWhatIsLeft(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker, err := New(Config{Token: "trk_test", Endpoint: server.URL, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	visit(tracker.Middleware(page(200, "<title>Home</title>")), http.MethodGet, "/")

	if err := tracker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

func TestWithoutTokenNothingIsCounted(t *testing.T) {
	tracker, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}

	site := page(200, "<title>Home</title>")
	if handler := tracker.Middleware(site); handler == nil {
		t.Fatal("no handler")
	}

	if tracker.Enabled() {
		t.Error("enabled without token")
	}

	if err := tracker.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientIP(t *testing.T) {
	tracker := startTracker(t, "http://127.0.0.1:1", func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} })

	tests := []struct {
		name    string
		remote  string
		headers []string
		want    string
	}{
		{"direct", "203.0.113.9:4000", nil, "203.0.113.9"},
		{"untrusted forwarder", "203.0.113.9:4000", []string{"X-Forwarded-For", "198.51.100.1"}, "203.0.113.9"},
		{"trusted proxy", "10.0.0.2:4000", []string{"X-Forwarded-For", "198.51.100.1, 10.0.0.3"}, "198.51.100.1"},
		{"spoofed first hop", "10.0.0.2:4000", []string{"X-Forwarded-For", "1.1.1.1, 198.51.100.1"}, "198.51.100.1"},
		{"real ip", "10.0.0.2:4000", []string{"X-Real-IP", "198.51.100.7"}, "198.51.100.7"},
		{"forwarded", "10.0.0.2:4000", []string{"Forwarded", `for="[2001:db8::1]:443";proto=https`}, "2001:db8::1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = test.remote

			for i := 0; i+1 < len(test.headers); i += 2 {
				req.Header.Set(test.headers[i], test.headers[i+1])
			}

			if got := tracker.clientIP(req); got != test.want {
				t.Errorf("clientIP = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLanguage(t *testing.T) {
	for accept, want := range map[string]string{
		"nl-NL,nl;q=0.9,en;q=0.8": "nl",
		"en":                      "en",
		"*":                       "",
		"":                        "",
		"zh-Hant-TW":              "zh",
		"x-klingon":               "",
	} {
		if got := language(accept); got != want {
			t.Errorf("language(%q) = %q, want %q", accept, got, want)
		}
	}
}
