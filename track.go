// Package track counts the page views of a website for Track
// (https://track.savage-software.nl), without cookies and without anything
// in the visitor's browser. Wrap the site's handler with the middleware:
//
//	tracker, err := track.New(track.Config{Token: os.Getenv("TRACK_TOKEN")})
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer tracker.Close(context.Background())
//
//	http.ListenAndServe(":8080", tracker.Middleware(site))
//
// The tracker never makes a page wait or fail. In the request the
// middleware only copies a few strings and the start of the page, and hands
// them to a goroutine without waiting: when that goroutine is behind, the
// view is dropped rather than held. The goroutine reads the title, and a
// second one sends the views to Track in batches, every ten seconds, with
// a context and a timeout of its own. When Track cannot be reached, the
// views wait in memory, up to Config.QueueSize, and are tried again. A
// panic in the tracker is recovered and logged; the page is not counted.
//
// Counted are pages a person opens: GET requests answered with 200 and
// text/html, without a file extension in the path. Not counted as pages
// are files, prefetches, htmx fragments (boosted links do count), and pages
// that ask not to be kept: "X-Robots-Tag: noindex", "Cache-Control:
// private" or <meta name="robots" content="noindex">. Counted apart are
// pages answered with 404, so the dashboard lists the addresses visitors
// did not find; answers with 500 or more, the server errors visitors ran
// into; downloads, files with an extension of Config.Downloads; and the
// requests of bots, crawlers, link previews and scripts, by the user agent
// only.
//
// What leaves the site for each view: the host, the path, the page's
// <title>, how long the server took for it, the referring site (its host
// only), the utm_source, utm_medium and utm_campaign of the address (no
// other part of the query), the visitor's first language, the user agent and
// the IP address. Track looks up the country and city of the address and
// counts the visitor with a salt that changes every day, and stores neither
// the address nor the user agent: only totals per day.
//
// Without a token the tracker counts nothing, so a site runs the same in
// development without Track.
package track

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultEndpoint is where events are sent unless Config.Endpoint says
// otherwise.
const DefaultEndpoint = "https://track.savage-software.nl/api/v1/collect"

const (
	defaultFlushInterval = 10 * time.Second
	defaultBatchSize     = 250
	defaultQueueSize     = 10_000
	// sampleBuffer is how many requests may wait for the worker that makes
	// events of them; beyond it, views are dropped.
	sampleBuffer = 4096
	// sendTimeout bounds one post to Track, also with an HTTPClient
	// without a timeout of its own.
	sendTimeout = 15 * time.Second
	// maxBackoff bounds the wait between attempts while Track cannot be
	// reached.
	maxBackoff = 5 * time.Minute
	// MaxBatch is the most events Track accepts in one request.
	MaxBatch = 1000
)

// closeTimeout bounds Close when its context has no deadline, so a site
// that stops while Track cannot be reached does not hang.
var closeTimeout = 5 * time.Second

// Config configures a Tracker. Only Token is required.
type Config struct {
	// Token is the site's token, from the site's settings at Track. Without
	// one the tracker counts nothing.
	Token string
	// Endpoint is the address events are posted to; DefaultEndpoint when
	// empty.
	Endpoint string
	// Hostname is the site's host name, such as "example.com", for a site
	// behind a proxy that changes the Host header. Track counts only the
	// events of the site's domain and its subdomains, so views of
	// localhost or a test server never mix with the real ones.
	Hostname string
	// SiteName is removed from the page titles where the layout adds it,
	// such as "About · Example" or "Example | About", so the dashboard lists
	// the pages' own titles.
	SiteName string
	// TrustedProxies are the proxies, as addresses or CIDR ranges such as
	// "10.0.0.0/8", whose X-Forwarded-For, X-Real-IP or Forwarded header
	// names the visitor. Without them the visitor is the direct peer
	// (r.RemoteAddr), which is right when middleware such as go-router's
	// RealIP or chi's RealIP has already put the visitor's address there.
	TrustedProxies []string
	// ClientIP returns the visitor's address, for a site that knows it
	// better, such as go-webthings' frontend.ClientIP. It replaces
	// TrustedProxies.
	ClientIP func(*http.Request) string
	// Skip leaves requests it reports true for uncounted, such as those of
	// the site's own editors.
	Skip func(*http.Request) bool
	// Downloads are the file extensions, such as ".pdf", whose requests
	// count as downloads; DefaultDownloads when nil. An empty list counts
	// none.
	Downloads []string
	// FlushInterval is how long events wait before they are sent; ten
	// seconds when zero.
	FlushInterval time.Duration
	// BatchSize is the most events sent at once, at most MaxBatch; 250 when
	// zero. A full batch is sent right away.
	BatchSize int
	// QueueSize is the most events kept while Track cannot be reached;
	// newer ones are dropped beyond it. 10,000 when zero.
	QueueSize int
	// HTTPClient sends the events; a client with a 10 second timeout when
	// nil.
	HTTPClient *http.Client
	// Logger reports events that could not be sent; slog.Default() when
	// nil.
	Logger *slog.Logger
}

// Kind is what an event counts.
type Kind string

const (
	// Pageview is a page a visitor opened.
	Pageview Kind = "pageview"
	// NotFound is an address a visitor opened that answered 404.
	NotFound Kind = "notfound"
	// Custom is an event the site reports itself, such as a sign-up (see
	// Tracker.Event).
	Custom Kind = "event"
	// ServerError is a request a visitor made that the site answered with
	// 500 or more; Event.Status holds the status.
	ServerError Kind = "error"
	// Download is a file of Config.Downloads a visitor fetched.
	Download Kind = "download"
	// Bot is a request of a bot, crawler, link preview or script. It holds
	// the user agent, never an address.
	Bot Kind = "bot"
)

// DefaultDownloads are the extensions of the files that count as downloads
// unless Config.Downloads says otherwise.
var DefaultDownloads = []string{
	".pdf", ".zip", ".rar", ".7z", ".gz", ".tgz", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
	".odt", ".ods", ".odp", ".csv", ".epub", ".mp3", ".mp4", ".mov", ".dmg", ".exe", ".msi", ".apk",
	".ics", ".vcf",
}

// Event is one counted request, as it is sent to Track. Track keeps none of
// it as it is: it adds it to the totals of its day.
type Event struct {
	Kind Kind      `json:"kind"`
	Time time.Time `json:"time"`
	// Host is the site's host name, without port.
	Host string `json:"host"`
	// Path is the path of the address, without its query.
	Path  string `json:"path"`
	Title string `json:"title,omitempty"`
	// Name is the name of a Custom event.
	Name string `json:"name,omitempty"`
	// Referrer is the host of the site the visitor came from, without
	// "www.": empty for direct visits and for links within the site.
	Referrer string `json:"referrer,omitempty"`
	// Source, Medium and Campaign are the address's utm_source (or ref),
	// utm_medium and utm_campaign.
	Source   string `json:"source,omitempty"`
	Medium   string `json:"medium,omitempty"`
	Campaign string `json:"campaign,omitempty"`
	// Language is the first language the browser asks for, such as "nl".
	Language  string `json:"language,omitempty"`
	UserAgent string `json:"user_agent"`
	IP        string `json:"ip"`
	// Duration is how long the site took to make and send the page, in
	// microseconds: from the request reaching the middleware until the
	// handler returned. Zero when it was not measured.
	Duration int64 `json:"duration_us,omitempty"`
	// Status is the status of a ServerError or a Bot request.
	Status int `json:"status,omitempty"`
}

// Batch is the body of a post to the endpoint.
type Batch struct {
	Events []Event `json:"events"`
}

// Tracker counts a site's page views and sends them to Track. It is safe
// for concurrent use.
type Tracker struct {
	config    Config
	downloads map[string]bool
	proxies   []netip.Prefix
	client    *http.Client
	logger    *slog.Logger
	disabled  bool

	mu      sync.Mutex
	queue   []Event
	backoff time.Duration
	retryAt time.Time

	// samples are the requests the worker makes events of; pending counts
	// those it has not finished.
	samples chan sample
	pending atomic.Int64
	heads   sync.Pool

	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}
	worked  chan struct{}
	closed  atomic.Bool
	sending sync.Mutex

	sent    atomic.Uint64
	dropped atomic.Uint64
}

// New returns a Tracker and starts its goroutines. Close it when the site
// stops, so the events in memory are sent. A config without a token gives a
// tracker that counts nothing.
func New(config Config) (*Tracker, error) {
	t, err := newTracker(config)
	if err != nil || t.disabled {
		return t, err
	}

	go t.work()
	go t.run()

	return t, nil
}

// newTracker returns a Tracker without starting its goroutines.
func newTracker(config Config) (*Tracker, error) {
	t := &Tracker{
		config:  config,
		client:  config.HTTPClient,
		logger:  config.Logger,
		samples: make(chan sample, sampleBuffer),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		worked:  make(chan struct{}),
	}

	downloads := config.Downloads
	if downloads == nil {
		downloads = DefaultDownloads
	}

	t.downloads = map[string]bool{}
	for _, extension := range downloads {
		t.downloads[strings.ToLower("."+strings.TrimPrefix(extension, "."))] = true
	}

	t.heads.New = func() any {
		head := make([]byte, 0, maxHead)

		return &head
	}

	if t.logger == nil {
		t.logger = slog.Default()
	}

	if strings.TrimSpace(config.Token) == "" {
		t.disabled = true
		t.logger.Info("track: no token, page views are not counted")

		return t, nil
	}

	if t.config.Endpoint == "" {
		t.config.Endpoint = DefaultEndpoint
	}

	if t.config.FlushInterval <= 0 {
		t.config.FlushInterval = defaultFlushInterval
	}

	if t.config.BatchSize <= 0 {
		t.config.BatchSize = defaultBatchSize
	}

	t.config.BatchSize = min(t.config.BatchSize, MaxBatch)

	if t.config.QueueSize <= 0 {
		t.config.QueueSize = defaultQueueSize
	}

	if t.client == nil {
		t.client = &http.Client{Timeout: 10 * time.Second}
	}

	for _, proxy := range config.TrustedProxies {
		prefix, err := parsePrefix(proxy)
		if err != nil {
			return nil, fmt.Errorf("track: trusted proxy %q: %w", proxy, err)
		}

		t.proxies = append(t.proxies, prefix)
	}

	return t, nil
}

func parsePrefix(value string) (netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "/") {
		return netip.ParsePrefix(value)
	}

	addr, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, err
	}

	return netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()), nil
}

// Enabled reports whether the tracker counts, which it does with a token.
func (t *Tracker) Enabled() bool {
	return !t.disabled
}

// Sent is the number of events Track accepted.
func (t *Tracker) Sent() uint64 {
	return t.sent.Load()
}

// Dropped is the number of events the tracker gave up on, because its queue
// was full or Track refused them.
func (t *Tracker) Dropped() uint64 {
	return t.dropped.Load()
}

// add queues an event for the sender; a full queue drops it. Only the
// worker adds events, so a request never waits for the queue's lock.
func (t *Tracker) add(event Event) {
	t.mu.Lock()

	if len(t.queue) >= t.config.QueueSize {
		t.mu.Unlock()
		t.dropped.Add(1)

		return
	}

	t.queue = append(t.queue, event)
	full := len(t.queue) >= t.config.BatchSize
	t.mu.Unlock()

	if full {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
}

// run sends the queue every FlushInterval, and at once when a batch is full,
// until Close.
func (t *Tracker) run() {
	defer close(t.done)

	ticker := time.NewTicker(t.config.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
		case <-t.wake:
		}

		t.mu.Lock()
		waiting := time.Now().Before(t.retryAt)
		t.mu.Unlock()

		if waiting {
			continue
		}

		if err := t.flush(context.Background()); err != nil {
			t.logger.Warn("track: events not sent, trying again later", "error", err)
		}
	}
}

// Flush sends the views counted so far now, in batches, once the worker has
// made events of them. Events Track could not take because it was
// unreachable or busy go back to the queue; events it refused are dropped.
func (t *Tracker) Flush(ctx context.Context) error {
	if t.disabled {
		return nil
	}

	t.drain(ctx)

	return t.flush(ctx)
}

// drain makes events of the samples that wait, and waits for the one the
// worker is busy with, for at most a second.
func (t *Tracker) drain(ctx context.Context) {
	deadline := time.Now().Add(time.Second)

	for {
		select {
		case s := <-t.samples:
			t.process(s)

			continue
		default:
		}

		if t.pending.Load() <= 0 || time.Now().After(deadline) || ctx.Err() != nil {
			return
		}

		time.Sleep(time.Millisecond)
	}
}

// flush sends the queued events in batches.
func (t *Tracker) flush(ctx context.Context) error {
	t.sending.Lock()
	defer t.sending.Unlock()

	for {
		t.mu.Lock()
		n := min(len(t.queue), t.config.BatchSize)
		batch := append([]Event(nil), t.queue[:n]...)
		t.queue = t.queue[n:]
		t.mu.Unlock()

		if len(batch) == 0 {
			return nil
		}

		err := t.send(ctx, batch)

		var refused refusedError

		switch {
		case err == nil:
			t.sent.Add(uint64(len(batch)))
			t.mu.Lock()
			t.backoff = 0
			t.retryAt = time.Time{}
			t.mu.Unlock()
		case errors.As(err, &refused):
			t.dropped.Add(uint64(len(batch)))
			t.logger.Error("track: events refused", "status", refused.status, "message", refused.message)
		default:
			t.requeue(batch)

			return err
		}
	}
}

// requeue puts a batch that could not be sent back in front of the queue,
// as far as there is room, and waits longer before the next attempt.
func (t *Tracker) requeue(batch []Event) {
	t.mu.Lock()
	defer t.mu.Unlock()

	room := t.config.QueueSize - len(t.queue)
	if room < len(batch) {
		t.dropped.Add(uint64(len(batch) - max(room, 0)))
		batch = batch[:max(room, 0)]
	}

	t.queue = append(batch, t.queue...)

	t.backoff = min(max(t.backoff*2, t.config.FlushInterval), maxBackoff)
	t.retryAt = time.Now().Add(t.backoff)
}

// refusedError is an answer that sending again will not change, such as a
// wrong token.
type refusedError struct {
	status  int
	message string
}

func (e refusedError) Error() string {
	return fmt.Sprintf("track refused the events: %d %s", e.status, e.message)
}

func (t *Tracker) send(ctx context.Context, batch []Event) error {
	body, err := json.Marshal(Batch{Events: batch})
	if err != nil {
		return refusedError{message: err.Error()}
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.config.Token)
	req.Header.Set("User-Agent", "track-go/1")

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	message, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch {
	case resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("track answered %d", resp.StatusCode)
	default:
		return refusedError{status: resp.StatusCode, message: strings.TrimSpace(string(message))}
	}
}

// Close stops the tracker and sends what is left, within ctx, or within
// five seconds when ctx has no deadline: a site that stops while Track
// cannot be reached does not hang. Views counted after Close are dropped.
func (t *Tracker) Close(ctx context.Context) error {
	if t.disabled || !t.closed.CompareAndSwap(false, true) {
		return nil
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, closeTimeout)
		defer cancel()
	}

	close(t.stop)

	for _, done := range []chan struct{}{t.worked, t.done} {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return t.flush(ctx)
}
