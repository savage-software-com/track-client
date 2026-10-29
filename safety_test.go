package track

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hangingTrack is a Track that takes every request and never answers,
// until the test ends.
func hangingTrack(t *testing.T) *httptest.Server {
	t.Helper()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))

	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	return server
}

// A page never waits for Track, also while every post to Track hangs.
func TestPagesNeverWaitForTrack(t *testing.T) {
	server := hangingTrack(t)

	tracker, err := New(Config{Token: "trk_test", Endpoint: server.URL, FlushInterval: 5 * time.Millisecond, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}

	site := tracker.Middleware(page(200, "<title>Home</title>"))

	start := time.Now()

	for i := 0; i < 2000; i++ {
		visit(site, http.MethodGet, "/")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("2000 pages took %s while Track hangs", elapsed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	closed := time.Now()
	_ = tracker.Close(ctx)

	if elapsed := time.Since(closed); elapsed > time.Second {
		t.Errorf("Close took %s with a deadline of 200ms", elapsed)
	}
}

// Close without a deadline gives up after closeTimeout.
func TestCloseDoesNotHang(t *testing.T) {
	server := hangingTrack(t)

	previous := closeTimeout
	closeTimeout = 200 * time.Millisecond

	t.Cleanup(func() { closeTimeout = previous })

	tracker, err := New(Config{Token: "trk_test", Endpoint: server.URL, FlushInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	visit(tracker.Middleware(page(200, "<title>Home</title>")), http.MethodGet, "/")

	start := time.Now()
	if err := tracker.Close(context.Background()); err == nil {
		t.Error("Close reported success while Track hangs")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close took %s", elapsed)
	}
}

// When the worker is behind, a view is dropped rather than waited for.
func TestBusyWorkerDropsViews(t *testing.T) {
	tracker, err := newTracker(Config{Token: "trk_test", Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}

	// Nobody takes samples from an unbuffered channel.
	tracker.samples = make(chan sample)

	done := make(chan struct{})

	go func() {
		visit(tracker.Middleware(page(200, "<title>Home</title>")), http.MethodGet, "/")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the page waits for the tracker")
	}

	if tracker.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", tracker.Dropped())
	}
}

// A panic in the tracker, or in the site's own Skip or ClientIP, costs the
// view, never the page.
func TestPanicsDoNotBreakPages(t *testing.T) {
	for name, config := range map[string]Config{
		"skip":      {Skip: func(*http.Request) bool { panic("skip") }},
		"client ip": {ClientIP: func(*http.Request) string { panic("client ip") }},
	} {
		t.Run(name, func(t *testing.T) {
			config.Token, config.Endpoint, config.FlushInterval = "trk_test", "http://127.0.0.1:1", time.Hour
			config.Logger = discard()

			tracker, err := New(config)
			if err != nil {
				t.Fatal(err)
			}

			defer tracker.Close(context.Background())

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("User-Agent", browser)

			rec := httptest.NewRecorder()
			tracker.Middleware(page(200, "<title>Home</title>")).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Home") {
				t.Errorf("page = %d %q", rec.Code, rec.Body.String())
			}

			req = httptest.NewRequest(http.MethodPost, "/signup", nil)
			req.Header.Set("User-Agent", browser)
			tracker.Event(req, "signup")
		})
	}
}

// WebSockets and server-sent events get the site's own writer.
func TestUpgradesPassUntouched(t *testing.T) {
	tracker := startTracker(t, "http://127.0.0.1:1")

	for _, header := range [][2]string{{"Upgrade", "websocket"}, {"Accept", "text/event-stream"}} {
		original := httptest.NewRecorder()

		handler := tracker.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if w != http.ResponseWriter(original) {
				t.Errorf("%s: the handler got the tracker's writer", header[0])
			}
		}))

		req := httptest.NewRequest(http.MethodGet, "/live", nil)
		req.Header.Set("User-Agent", browser)
		req.Header.Set(header[0], header[1])
		handler.ServeHTTP(original, req)
	}
}

// A handler can still take over the connection of a counted page.
func TestHijackThroughTheRecorder(t *testing.T) {
	tracker := startTracker(t, "http://127.0.0.1:1")

	server := httptest.NewServer(tracker.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)

			return
		}

		defer conn.Close()

		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\nhijacked")
		_ = buf.Flush()
	})))
	defer server.Close()

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/page", nil)
	req.Header.Set("User-Agent", browser)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hijacked" {
		t.Errorf("body = %q", body)
	}

	// A writer that cannot be hijacked says so, as without the tracker.
	rec := &recorder{ResponseWriter: httptest.NewRecorder(), heads: &tracker.heads}
	if _, _, err := rec.Hijack(); !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("hijack of a recorder = %v, want ErrNotSupported", err)
	}
}

// An Early Hints answer is not taken for the page's status.
func TestEarlyHints(t *testing.T) {
	fake := &collector{}
	server := httptest.NewServer(fake)
	defer server.Close()

	tracker := startTracker(t, server.URL)
	site := tracker.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>Hinted</title>")
	}))

	visit(site, http.MethodGet, "/hinted")

	if err := tracker.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if events := fake.received(); len(events) != 1 || events[0].Title != "Hinted" {
		t.Errorf("events = %+v", events)
	}
}

// BenchmarkMiddleware is what counting costs a page, against the page alone.
func BenchmarkMiddleware(b *testing.B) {
	body := "<html><head><title>Benchmark · Site</title></head><body>" + strings.Repeat("<p>text</p>", 2000) + "</body></html>"
	handler := page(200, body)

	tracker, err := New(Config{Token: "trk_test", Endpoint: "http://127.0.0.1:1", FlushInterval: time.Hour, Logger: discard()})
	if err != nil {
		b.Fatal(err)
	}

	defer tracker.Close(context.Background())

	req := httptest.NewRequest(http.MethodGet, "/page?utm_source=news", nil)
	req.Header.Set("User-Agent", browser)
	req.Header.Set("Accept-Language", "nl-NL,nl;q=0.9")
	req.Header.Set("Referer", "https://www.google.com/")

	for _, bench := range []struct {
		name    string
		handler http.Handler
	}{
		{"page", handler},
		{"page with tracker", tracker.Middleware(handler)},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				bench.handler.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}

// discard is a logger for tests whose panics are expected.
func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
