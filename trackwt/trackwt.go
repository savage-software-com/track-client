// Package trackwt adds Track to a go-webthings site as a module, next to
// or instead of the site's own stats module:
//
//	app.Use(trackwt.Module(track.Config{Token: os.Getenv("TRACK_TOKEN")}))
//
// It counts what the stats module counts: the public pages, not the admin,
// the visitor actions, media and assets, and not the editors who are
// signed in. The visitor's address is the one go-webthings found
// (frontend.ClientIP), so list a reverse proxy in Config.TrustedProxies of
// the app. Closing the app sends the views that are left.
package trackwt

import (
	"net/http"

	webthings "github.com/donseba/go-webthings"
	"github.com/donseba/go-webthings/frontend"

	track "github.com/savage-software-com/track-client"
)

// Module returns the module with the key "track". config is used as
// track.New takes it, with the visitor's address and the editors from
// go-webthings.
func Module(config track.Config) webthings.Module {
	return webthings.Module{
		Type:  "track",
		Key:   "track",
		Title: "Track",
		Setup: func(env webthings.Env) (webthings.Parts, error) {
			if config.ClientIP == nil {
				config.ClientIP = frontend.ClientIP
			}

			skip := config.Skip
			config.Skip = func(r *http.Request) bool {
				if _, editor := frontend.Editor(r.Context()); editor {
					return true
				}

				return skip != nil && skip(r)
			}

			tracker, err := track.New(config)
			if err != nil {
				return webthings.Parts{}, err
			}

			return webthings.Parts{
				Service:    tracker,
				Middleware: tracker.Middleware,
				Close:      tracker.Close,
			}, nil
		},
	}
}

// Of returns the tracker of a started app, for custom events
// (track.Tracker.Event), or nil.
func Of(app *webthings.App) *track.Tracker {
	tracker, _ := app.Service("track").(*track.Tracker)

	return tracker
}
