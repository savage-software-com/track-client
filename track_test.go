package track

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func newTracker(t *testing.T, endpoint string, configure ...func(*Config)) *Tracker {
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

	tracker := newTracker(t, server.URL, func(c *Config) { c.SiteName = "Example" })
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
	want := Event{
		Kind: Pageview, Host: "example.com", Path: "/about", Title: "About & us", Referrer: "google.com",
		Source: "news", Campaign: "Autumn sale", Language: "nl", UserAgent: browser, IP: "192.0.2.1",
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

	tracker := newTracker(t, server.URL, func(c *Config) {
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

	events := fake.received()
	if len(events) != 1 || events[0].Path != "/boosted" {
		t.Fatalf("events = %+v, want only /boosted", events)
	}
}

func TestMiddlewareCountsNotFound(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := newTracker(t, server.URL)
	visit(tracker.Middleware(page(404, "<title>Not found</title>")), http.MethodGet, "/old-page")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := fake.received()
	if len(events) != 1 || events[0].Kind != NotFound || events[0].Path != "/old-page" {
		t.Fatalf("events = %+v", events)
	}
}

func TestCustomEvent(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := newTracker(t, server.URL)

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

	tracker := newTracker(t, server.URL)
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

	tracker := newTracker(t, server.URL)
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
	tracker := newTracker(t, "http://127.0.0.1:1", func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/8"} })

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
