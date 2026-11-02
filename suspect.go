package track

import (
	"net/http"
	"strings"
)

// SuspectHeaders is the value of Event.Suspect for a request that calls itself
// a modern browser, but lacks the headers that browser always sends: most likely
// a program dressed up as one. Such a visit still counts; Track shows how many
// there are, so its owner can see what the visitors are worth.
const SuspectHeaders = "headers"

// lacksBrowserHeaders reports whether r claims a browser that sends fetch
// metadata, without it. Browsers only send it over HTTPS, and only from these
// versions on, so older ones and plain HTTP say nothing. It looks at nothing but
// whether these headers are there.
func lacksBrowserHeaders(r *http.Request) bool {
	if !overHTTPS(r) {
		return false
	}

	agent := r.UserAgent()
	metadata := r.Header.Get("Sec-Fetch-Mode") != ""

	switch {
	case strings.Contains(agent, "Chrome/"):
		chrome := majorVersion(agent, "Chrome/")

		// An Android WebView, marked "wv", need not send client hints.
		hints := chrome < 90 || strings.Contains(agent, "; wv)") || r.Header.Get("Sec-Ch-Ua") != ""

		return chrome >= 80 && (!metadata || !hints)
	case strings.Contains(agent, "Firefox/"):
		return majorVersion(agent, "Firefox/") >= 90 && !metadata
	case strings.Contains(agent, "Safari/") && strings.Contains(agent, "Version/"):
		return majorVersion(agent, "Version/") >= 17 && !metadata
	default:
		return false
	}
}

// overHTTPS reports whether the browser reached the site over HTTPS, as far as
// this server can tell, also behind a proxy.
func overHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// majorVersion is the number after token in agent, or 0.
func majorVersion(agent, token string) int {
	_, rest, found := strings.Cut(agent, token)
	if !found {
		return 0
	}

	major := 0

	for i := 0; i < len(rest) && i < 6 && rest[i] >= '0' && rest[i] <= '9'; i++ {
		major = major*10 + int(rest[i]-'0')
	}

	return major
}
