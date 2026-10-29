# track-client

The Go client of [Track](https://track.savage-software.nl): page view
statistics for your website without cookies, without a script in the page
and without storing IP addresses.

The middleware counts the pages visitors open on your server and sends them
to Track in batches, every ten seconds. A page never waits for Track; when
Track cannot be reached, the views wait in memory and are sent later.

```sh
go get github.com/savage-software-com/track-client
```

```go
import track "github.com/savage-software-com/track-client"

tracker, err := track.New(track.Config{Token: os.Getenv("TRACK_TOKEN")})
if err != nil {
	log.Fatal(err)
}
defer tracker.Close(context.Background())

http.ListenAndServe(":8080", tracker.Middleware(handler))
```

Get the token by registering your domain at
<https://track.savage-software.nl>. Without a token the tracker counts
nothing, so development runs without Track. The package has no dependencies
besides the standard library.

## go-webthings

A [go-webthings](https://github.com/donseba/go-webthings) site adds the
module, next to or instead of its `stats` module. It counts the public pages
only, not the admin and not signed-in editors, with the visitor's address as
go-webthings found it:

```sh
go get github.com/savage-software-com/track-client/trackwt
```

```go
app.Use(trackwt.Module(track.Config{Token: os.Getenv("TRACK_TOKEN")}))
```

`trackwt` is a module of its own, so sites without go-webthings do not get
its dependencies.

## What is counted

Pages a person opens: GET requests answered with 200 and `text/html`, without
a file extension in the path. Not counted are files, form posts, prefetches,
htmx fragments (boosted links do count), bots, link previews, monitors and
scripts, and pages that ask not to be kept: `X-Robots-Tag: noindex`,
`Cache-Control: private` or `<meta name="robots" content="noindex">`. Pages
answered with 404 are counted apart, so the dashboard lists the addresses
visitors did not find.

Your own events, such as a sign-up, count with `Event`:

```go
tracker.Event(r, "newsletter sign-up")
```

## What is sent

For each view: the host, the path without the query, the page's `<title>`,
the host of the referring site, `utm_source` (or `ref`), `utm_medium` and
`utm_campaign`, the browser's first language, the user agent and the IP
address. Track looks up the country and city of the address and recognises
the visitor for that day with a salted hash; the salt differs per site and
per day and lives in memory only. Track stores neither the address nor the
user agent: only totals per day, each dimension on its own.

## Configuration

| Field | |
| --- | --- |
| `Token` | The site's token. |
| `Endpoint` | Where events go; `https://track.savage-software.nl/api/v1/collect` by default. |
| `Hostname` | The site's host name, behind a proxy that changes the Host header. Track counts only events of the registered domain and its subdomains. |
| `SiteName` | Removed from page titles where the layout adds it: `About · My site` becomes `About`. |
| `TrustedProxies` | Proxies (addresses or CIDR ranges) whose `X-Forwarded-For`, `X-Real-IP` or `Forwarded` header names the visitor. Without them the visitor is `r.RemoteAddr`, which is right behind middleware such as go-router's or chi's `RealIP`. |
| `ClientIP` | A function that returns the visitor's address, instead of the above. |
| `Skip` | A function that leaves requests uncounted, such as your editors'. |
| `FlushInterval`, `BatchSize`, `QueueSize` | 10 seconds, 250 events and 10,000 events by default. |
| `HTTPClient`, `Logger` | A client with a 10 second timeout and `slog.Default()` by default. |

## The endpoint

Events are posted to the endpoint as `{"events": [...]}` (`track.Batch`),
at most 1000 per request, with `Authorization: Bearer <token>`. Track answers
`202` with the number of events it accepted and rejected; `401` for an
unknown token.
