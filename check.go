package track

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

const (
	// CheckHeader marks a request of Track's security check, signed with
	// SignCheck. Without a valid signature it means nothing.
	CheckHeader = "Track-Check"
	// CheckProofHeader answers it: proof that this server holds the site's
	// token, so that the check is looking at the right website.
	CheckProofHeader = "Track-Check-Proof"
	// CheckPath answers a signed request with the server's CheckReport.
	CheckPath = "/track-check"

	// checkWindow is how far a signature's time may be from now.
	checkWindow = 10 * time.Minute
)

// CheckReport is what a server tells Track's security check: what it is built
// with, to compare with known vulnerabilities. It says nothing about visitors.
type CheckReport struct {
	Go      string        `json:"go"`
	Modules []CheckModule `json:"modules"`
}

// CheckModule is a Go module in the server's build.
type CheckModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// TokenKey is the key of the signatures for a token: what Track stores of it.
func TokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// SignCheck is the value of CheckHeader for a request at the given time.
func SignCheck(key string, at time.Time, nonce string) string {
	stamp := strconv.FormatInt(at.Unix(), 10)

	return stamp + "." + nonce + "." + checkMAC(key, "request", stamp, nonce)
}

// CheckProof is the value of CheckProofHeader for a request with this nonce.
func CheckProof(key, nonce string) string {
	return checkMAC(key, "proof", nonce)
}

func checkMAC(key string, parts ...string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(strings.Join(parts, "\n")))

	return hex.EncodeToString(mac.Sum(nil))
}

// verifyCheck reports whether value is a valid signature from Track, and the
// proof to answer it with. A false one counts as an ordinary request: it cannot
// be used to stay out of the statistics.
func (t *Tracker) verifyCheck(value string, now time.Time) (string, bool) {
	stamp, rest, ok := strings.Cut(value, ".")
	if !ok {
		return "", false
	}

	nonce, signature, ok := strings.Cut(rest, ".")
	if !ok {
		return "", false
	}

	seconds, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return "", false
	}

	if age := now.Sub(time.Unix(seconds, 0)); age > checkWindow || age < -checkWindow {
		return "", false
	}

	key := TokenKey(t.config.Token)
	if !hmac.Equal([]byte(signature), []byte(checkMAC(key, "request", stamp, nonce))) {
		return "", false
	}

	return CheckProof(key, nonce), true
}

// checkRequest answers a signed request of Track's security check, which is not
// counted. It reports whether it handled the request to the end.
func (t *Tracker) checkRequest(w http.ResponseWriter, r *http.Request) (checked, done bool) {
	value := r.Header.Get(CheckHeader)
	if value == "" {
		return false, false
	}

	proof, ok := t.verifyCheck(value, time.Now())
	if !ok {
		return false, false
	}

	w.Header().Set(CheckProofHeader, proof)

	if r.URL.Path != CheckPath {
		return true, false
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(buildReport()); err != nil {
		t.logger.Error("track: check report", "error", fmt.Errorf("encode: %w", err))
	}

	return true, true
}

// buildReport reads what the server is built with from the binary itself.
func buildReport() CheckReport {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return CheckReport{}
	}

	report := CheckReport{Go: info.GoVersion}

	for _, dep := range info.Deps {
		if dep.Replace != nil {
			dep = dep.Replace
		}

		report.Modules = append(report.Modules, CheckModule{Path: dep.Path, Version: dep.Version})
	}

	return report
}
