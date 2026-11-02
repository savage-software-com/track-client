package track

import (
	"fmt"
	"html"
	"net/http"
	"strings"
)

const (
	// DefaultOptOutPath is where a visitor can choose not to be counted,
	// unless Config.OptOutPath says otherwise. Link to it from your site.
	DefaultOptOutPath = "/track-opt-out"
	// OptOutCookie remembers that choice. It is the only cookie the tracker
	// ever sets, and only when a visitor asks for it.
	OptOutCookie = "track-opt-out"

	// optOutAge is how long the choice lasts, in seconds: a year.
	optOutAge = 365 * 24 * 60 * 60
)

// optsOut reports whether r comes from a visitor who does not want to be
// counted: by the choice on the opt-out page, or by what the browser says for
// every site.
func optsOut(r *http.Request) bool {
	return signalsOptOut(r) || choseOptOut(r)
}

// signalsOptOut reports whether the browser asks every site not to track it,
// with Global Privacy Control or Do Not Track.
func signalsOptOut(r *http.Request) bool {
	return r.Header.Get("Sec-GPC") == "1" || r.Header.Get("DNT") == "1"
}

func choseOptOut(r *http.Request) bool {
	cookie, err := r.Cookie(OptOutCookie)

	return err == nil && cookie.Value == "1"
}

// optOut answers the opt-out page: it shows the choice, and a post changes it.
func (t *Tracker) optOut(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	case http.MethodPost:
		t.setOptOut(w, r, r.PostFormValue("choice") == "out")
		http.Redirect(w, r, t.config.OptOutPath, http.StatusSeeOther)

		return
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)

		return
	}

	text := optOutTextFor(language(r.Header.Get("Accept-Language")))

	status, choice, button := text.counted, "out", text.stop

	switch {
	case signalsOptOut(r):
		status, choice, button = text.signal, "", ""
	case choseOptOut(r):
		status, choice, button = text.notCounted, "in", text.resume
	}

	form := ""
	if choice != "" {
		form = fmt.Sprintf(`<form method="post"><input type="hidden" name="choice" value="%s"><button type="submit">%s</button></form>`, choice, html.EscapeString(button))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")

	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="%s">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>%s</title>
<body>
<main>
<h1>%s</h1>
<p>%s</p>
<p><strong>%s</strong></p>
%s
<p>%s</p>
<p><a href="/">%s</a></p>
</main>
</body>
</html>
`,
		text.lang,
		html.EscapeString(text.title), html.EscapeString(text.title), html.EscapeString(text.lead),
		html.EscapeString(status), form, html.EscapeString(text.note), html.EscapeString(text.back))
}

// setOptOut sets or clears the cookie of the choice.
func (t *Tracker) setOptOut(w http.ResponseWriter, r *http.Request, out bool) {
	cookie := &http.Cookie{
		Name:     OptOutCookie,
		Value:    "1",
		Path:     "/",
		Domain:   t.config.OptOutDomain,
		MaxAge:   optOutAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	}

	if !out {
		cookie.Value, cookie.MaxAge = "", -1
	}

	http.SetCookie(w, cookie)
}

// optOutText is the opt-out page in one language.
type optOutText struct {
	lang, title, lead, counted, notCounted, signal, stop, resume, note, back string
}

func optOutTextFor(lang string) optOutText {
	if text, ok := optOutTexts[lang]; ok {
		return text
	}

	return optOutTexts["en"]
}

var optOutTexts = map[string]optOutText{
	"en": {
		lang:       "en",
		title:      "Do not count my visits",
		lead:       "This website counts its visits with Track, on its own server, without cookies or scripts. If you would rather not be counted, your browser can remember that with one cookie, which does nothing else, for a year.",
		counted:    "Your visits are being counted.",
		notCounted: "Your visits are not being counted.",
		signal:     "Your browser asks websites not to track you (Global Privacy Control or Do Not Track), so your visits are not being counted.",
		stop:       "Stop counting my visits",
		resume:     "Count my visits again",
		note:       "This works from now on, in this browser. Track keeps only totals per day and does not recognise you the next day, so earlier visits cannot be taken out.",
		back:       "Back to the website",
	},
	"nl": {
		lang:       "nl",
		title:      "Mijn bezoeken niet meetellen",
		lead:       "Deze website telt haar bezoeken met Track, op de eigen server, zonder cookies of scripts. Wil je niet meegeteld worden? Dan kan je browser dat onthouden met één cookie, die verder niets doet, een jaar lang.",
		counted:    "Je bezoeken worden nu meegeteld.",
		notCounted: "Je bezoeken worden nu niet meegeteld.",
		signal:     "Je browser vraagt websites om je niet te volgen (Global Privacy Control of Do Not Track), dus je bezoeken worden niet meegeteld.",
		stop:       "Tel mijn bezoeken niet mee",
		resume:     "Tel mijn bezoeken weer mee",
		note:       "Dit geldt vanaf nu, in deze browser. Track bewaart alleen totalen per dag en herkent je de volgende dag niet, dus eerdere bezoeken kunnen er niet uit.",
		back:       "Terug naar de website",
	},
	"it": {
		lang:       "it",
		title:      "Non contare le mie visite",
		lead:       "Questo sito conta le visite con Track, sul proprio server, senza cookie né script. Se preferisci non essere contato, il tuo browser può ricordarlo per un anno con un solo cookie, che non fa nient’altro.",
		counted:    "Le tue visite vengono contate.",
		notCounted: "Le tue visite non vengono contate.",
		signal:     "Il tuo browser chiede ai siti di non tracciarti (Global Privacy Control o Do Not Track), quindi le tue visite non vengono contate.",
		stop:       "Non contare le mie visite",
		resume:     "Conta di nuovo le mie visite",
		note:       "Vale da ora in poi, in questo browser. Track conserva solo totali per giorno e il giorno dopo non ti riconosce, quindi le visite precedenti non si possono togliere.",
		back:       "Torna al sito",
	},
	"de": {
		lang:       "de",
		title:      "Meine Besuche nicht zählen",
		lead:       "Diese Website zählt ihre Besuche mit Track, auf dem eigenen Server, ohne Cookies oder Skripte. Wenn du nicht gezählt werden möchtest, kann sich dein Browser das ein Jahr lang mit einem einzigen Cookie merken, das sonst nichts tut.",
		counted:    "Deine Besuche werden gezählt.",
		notCounted: "Deine Besuche werden nicht gezählt.",
		signal:     "Dein Browser bittet Websites, dich nicht zu verfolgen (Global Privacy Control oder Do Not Track), daher werden deine Besuche nicht gezählt.",
		stop:       "Meine Besuche nicht mehr zählen",
		resume:     "Meine Besuche wieder zählen",
		note:       "Das gilt ab jetzt, in diesem Browser. Track speichert nur Summen pro Tag und erkennt dich am nächsten Tag nicht wieder, daher lassen sich frühere Besuche nicht herausnehmen.",
		back:       "Zurück zur Website",
	},
	"fr": {
		lang:       "fr",
		title:      "Ne pas compter mes visites",
		lead:       "Ce site compte ses visites avec Track, sur son propre serveur, sans cookies ni scripts. Si vous préférez ne pas être compté, votre navigateur peut s’en souvenir pendant un an grâce à un seul cookie, qui ne fait rien d’autre.",
		counted:    "Vos visites sont comptées.",
		notCounted: "Vos visites ne sont pas comptées.",
		signal:     "Votre navigateur demande aux sites de ne pas vous suivre (Global Privacy Control ou Do Not Track), vos visites ne sont donc pas comptées.",
		stop:       "Ne plus compter mes visites",
		resume:     "Compter à nouveau mes visites",
		note:       "Cela s’applique à partir de maintenant, dans ce navigateur. Track ne conserve que des totaux par jour et ne vous reconnaît pas le lendemain, les visites précédentes ne peuvent donc pas être retirées.",
		back:       "Retour au site",
	},
	"es": {
		lang:       "es",
		title:      "No contar mis visitas",
		lead:       "Este sitio cuenta sus visitas con Track, en su propio servidor, sin cookies ni scripts. Si prefieres que no te cuenten, tu navegador puede recordarlo durante un año con una sola cookie, que no hace nada más.",
		counted:    "Tus visitas se están contando.",
		notCounted: "Tus visitas no se están contando.",
		signal:     "Tu navegador pide a los sitios que no te rastreen (Global Privacy Control o Do Not Track), así que tus visitas no se cuentan.",
		stop:       "No contar más mis visitas",
		resume:     "Volver a contar mis visitas",
		note:       "Se aplica a partir de ahora, en este navegador. Track solo guarda totales por día y no te reconoce al día siguiente, así que las visitas anteriores no se pueden quitar.",
		back:       "Volver al sitio",
	},
}
