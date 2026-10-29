package track

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sample is what the middleware keeps of a request for the worker: strings
// copied from the request, never the request itself, and the start of the
// page, so the request has nothing more to do with the tracker once it
// handed the sample over.
type sample struct {
	kind     Kind
	at       time.Time
	duration time.Duration

	host     string
	path     string
	query    string
	referer  string
	language string
	agent    string
	ip       string
	name     string

	// encoding, robots and cache are the response's Content-Encoding,
	// X-Robots-Tag and Cache-Control.
	encoding string
	robots   []string
	cache    []string
	// head is the start of the page, from the tracker's pool.
	head *[]byte
}

// sample copies what the event of r needs.
func (t *Tracker) sample(r *http.Request, kind Kind) sample {
	return sample{
		kind:     kind,
		at:       time.Now().UTC(),
		host:     t.host(r),
		path:     r.URL.Path,
		query:    r.URL.RawQuery,
		referer:  r.Referer(),
		language: r.Header.Get("Accept-Language"),
		agent:    r.UserAgent(),
		ip:       t.clientIP(r),
	}
}

// enqueue hands s to the worker without waiting: when the worker is
// behind, or the tracker closed, the view is dropped.
func (t *Tracker) enqueue(s sample) {
	if t.closed.Load() {
		t.release(s)

		return
	}

	t.pending.Add(1)

	select {
	case t.samples <- s:
	default:
		t.pending.Add(-1)
		t.dropped.Add(1)
		t.release(s)
	}
}

func (t *Tracker) release(s sample) {
	if s.head != nil {
		t.heads.Put(s.head)
	}
}

// work makes events of the samples until Close, and then of the samples
// that are left.
func (t *Tracker) work() {
	defer close(t.worked)

	for {
		select {
		case s := <-t.samples:
			t.process(s)
		case <-t.stop:
			for {
				select {
				case s := <-t.samples:
					t.process(s)
				default:
					return
				}
			}
		}
	}
}

// process makes the event of a sample and queues it. A panic is recovered,
// so the tracker's goroutine, and with it the site, keeps running.
func (t *Tracker) process(s sample) {
	defer t.pending.Add(-1)
	defer t.release(s)

	defer func() {
		if v := recover(); v != nil {
			t.logger.Error("track: page not counted", "panic", v)
		}
	}()

	if isBot(s.agent) {
		return
	}

	event := t.eventOf(s)

	switch s.kind {
	case Pageview:
		var head []byte
		if s.head != nil {
			head = plainHead(s.encoding, *s.head)
		}

		if private(s.robots, s.cache) || noindexMeta(head) {
			return
		}

		event.Title = withoutSiteName(title(head), t.config.SiteName)
	case Custom:
		event.Name = truncate(strings.Join(strings.Fields(s.name), " "), maxText)
		if event.Name == "" {
			return
		}
	}

	t.add(event)
}

// eventOf is the event of a sample, as it is sent.
func (t *Tracker) eventOf(s sample) Event {
	query, _ := url.ParseQuery(s.query)

	source := query.Get("utm_source")
	if source == "" {
		source = query.Get("ref")
	}

	event := Event{
		Kind:      s.kind,
		Time:      s.at,
		Host:      s.host,
		Path:      s.path,
		Referrer:  referrer(s.referer, s.host),
		Source:    field(source),
		Medium:    field(query.Get("utm_medium")),
		Campaign:  field(query.Get("utm_campaign")),
		Language:  language(s.language),
		UserAgent: s.agent,
		IP:        s.ip,
	}

	if s.kind != Custom {
		event.Duration = microseconds(s.duration)
	}

	return event
}
