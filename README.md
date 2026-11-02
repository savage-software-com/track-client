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

## Visitors can object

Track counts visitors without asking, so they should be able to say no. The
middleware answers `/track-opt-out` (`Config.OptOutPath`), a small page in the
visitor's language with a button: link to it from your footer or privacy
statement. The choice is kept in one cookie, `track-opt-out`, which is only set
when a visitor asks for it, and is the only cookie the tracker ever sets. A
browser that sends Global Privacy Control (`Sec-GPC: 1`) or Do Not Track
(`DNT: 1`) is not counted either. An objection works from then on: earlier
visits are only in totals per day. Only visitors can object: bots and scan
attempts are counted without anything about a person, whatever they send.

## Security check

If you turn it on in Track, Track looks at your site from outside once a day.
Its requests carry a signature made with your token: the middleware answers
with proof that it holds the same token, so Track only looks at sites it knows
are yours, and these requests are not counted. A signed request for
`/track-check` is also told the Go version and modules the server is built
with, from the binary itself, to compare with known vulnerabilities. Nothing
about visitors is in it, and without the signature that address is like any
other.

## What is counted

- **Pages** a person opens: GET requests answered with 200 and `text/html`,
  named by your handler or worked out by Go from the start of the page,
  without a file extension in the path. Not counted are files, form posts,
  prefetches, htmx fragments (boosted links do count), and pages that ask
  not to be kept: `X-Robots-Tag: noindex`, `Cache-Control: private` or
  `<meta name="robots" content="noindex">`.
- **Pages not found:** the same requests answered with 404, as a page or as
  plain text such as Go's own `404 page not found`, but not as JSON, so the
  dashboard lists the addresses visitors did not find.
- **Server errors:** requests a visitor made that were answered with 500 or
  more, by path.
- **Downloads:** files with an extension of `Downloads` (`.pdf`, `.zip`,
  `.docx` and so on by default), answered whole or from their first byte, so
  a PDF viewer that fetches a file in parts counts once.
- **Bots, crawlers and scripts**, such as Googlebot, GPTBot, ClaudeBot or
  curl, and AI assistants that fetch a page for their user (ChatGPT-User,
  Claude-User): by their user agent only, never as visitors. Feed readers
  such as Feedly and Inoreader are counted on feeds too (`/feed.xml`), with
  the number of subscribers their user agent reports.

- **Scan attempts:** addresses scanners try on every website, such as
  `/.env`, `/.git/config` or `/wp-login.php`, in any method and whoever asks.
  Only the path and the status are sent, nothing about who asked, so they count
  also when the request sends Global Privacy Control or Do Not Track. A page
  your site really has at such an address still counts as a page.

Your own events, such as a sign-up, count with `Event`:

```go
tracker.Event(r, "newsletter sign-up")
```

## What is sent

For a bot, only its user agent, the host and the path. For each view, 404,
server error and download: the host, the path without the query, the page's
`<title>`, how long your server took to make and send it, the host of the
referring site or, for a link within your site, the path of the page it was
on, `utm_source` (or `ref`), `utm_medium` and `utm_campaign`, the browser's
first language, the user agent and the visitor's network: the IP address
without its last part (`MaskIP`), so the full address never leaves your
server. Track looks up the country and city of the network and recognises the
visitor for that day with a salted hash; the salt differs per site and per
day and lives in memory only. Track stores neither the network nor the user
agent: only totals per day.

## Recognising programs dressed up as browsers

Modern browsers always send fetch metadata such as `Sec-Fetch-Mode`, and Chrome
also client hints such as `Sec-Ch-Ua`, over HTTPS. A request that claims such a
browser without them gets `suspect: "headers"` in its event, so Track can show how
many there are. It looks only at whether these headers are there; none of their
values leave the server, and the visit still counts.

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
| `OptOutPath`, `OptOutDomain` | The address of the page where a visitor can object, `/track-opt-out` by default, and the domain of its cookie for a site on several subdomains. |
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
