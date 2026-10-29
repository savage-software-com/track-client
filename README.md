# track-client

The Go client of [Track](https://track.savage-software.nl): page view
statistics for your website without cookies, without a script in the page
and without storing IP addresses.

The middleware counts the pages visitors open on your server and sends them
to Track in batches, every ten seconds.

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

## It never slows your site down

- In the request, the middleware only copies a few strings and the start of
  the page, and hands them to a goroutine without waiting: about 3 µs per
  page. When that goroutine is behind, the view is dropped, never held.
- Reading the title and sending happen in goroutines of their own, with a
  context and a timeout of their own (15 seconds per post). A slow or
  unreachable Track never reaches a request; the views wait in memory, up
  to `QueueSize`, and are tried again later.
- A panic in the tracker, or in your own `Skip` or `ClientIP`, is recovered
  and logged: the view is lost, the page is not.
- WebSockets and server-sent events are passed through untouched, and a
  handler can still hijack the connection or flush a stream.
- `Close` gives up after five seconds when its context has no deadline, so
  a site that stops while Track is down stops on time.

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

- **Pages** a person opens: GET requests answered with 200 and `text/html`,
  without a file extension in the path. Not counted are files, form posts,
  prefetches, htmx fragments (boosted links do count), and pages that ask
  not to be kept: `X-Robots-Tag: noindex`, `Cache-Control: private` or
  `<meta name="robots" content="noindex">`.
- **Pages not found:** the same requests answered with 404, so the dashboard
  lists the addresses visitors did not find.
- **Server errors:** requests a visitor made that were answered with 500 or
  more, by path.
- **Downloads:** files with an extension of `Downloads` (`.pdf`, `.zip`,
  `.docx` and so on by default), answered whole or from their first byte, so
  a PDF viewer that fetches a file in parts counts once.
- **Bots, crawlers and scripts**, such as Googlebot, GPTBot, ClaudeBot or
  curl, and AI assistants that fetch a page for their user (ChatGPT-User,
  Claude-User): by their user agent only, never as visitors.

Your own events, such as a sign-up, count with `Event`:

```go
tracker.Event(r, "newsletter sign-up")
```

## What is sent

For a bot, only its user agent, the host and the path. For each view, 404,
server error and download: the host, the path without the query, the page's
`<title>`, how long your server took to make and send it, the host of the
referring site, `utm_source` (or `ref`), `utm_medium` and `utm_campaign`,
the browser's first language, the user agent and the IP address. Track looks
up the country and city of the address and recognises the visitor for that
day with a salted hash; the salt differs per site and per day and lives in
memory only. Track stores neither the address nor the user agent: only
totals per day, each dimension on its own.

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
| `Downloads` | The file extensions that count as downloads; `DefaultDownloads` when nil, none when empty. |
| `FlushInterval`, `BatchSize`, `QueueSize` | 10 seconds, 250 events and 10,000 events by default. |
| `HTTPClient`, `Logger` | A client with a 10 second timeout and `slog.Default()` by default. |

## The endpoint

Events are posted to the endpoint as `{"events": [...]}` (`track.Batch`),
at most 1000 per request, with `Authorization: Bearer <token>`. Track answers
`202` with the number of events it accepted and rejected; `401` for an
unknown token.

## License

The client is source-available, not open source: you may use it, unmodified,
only to send statistics to Track, under an account there. See
[LICENSE](LICENSE). Because the license is not an open source license,
pkg.go.dev does not show the documentation; this README and the comments in
the code do.
