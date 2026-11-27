package panel

import (
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

const usageOnlyAddress = "balance.atavix.invalid:1"

var usageOnlyID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(usageOnlyAddress))

type subscriptionPage struct {
	User                             User
	Profiles                         []connectionProfile
	ShareURL                         string
	CopyList                         string
	AndroidLink, HiddifySubscription template.URL
	TelegramLink                     template.URL
}

// Both clients register this action. v2rayNG also parses a URL-safe Base64 list.
func clientImport(client, payload string) template.URL {
	action := url.URL{
		Scheme: client, Host: "install-config",
		RawQuery: url.Values{"url": {payload}}.Encode(),
	}
	return template.URL(action.String())
}

func subscriptionBalance(remaining, quota int64) string {
	scale, unit := int64(1), "B"
	switch {
	case quota >= 1_000_000_000:
		scale, unit = 1_000_000_000, "GB"
	case quota >= 1_000_000:
		scale, unit = 1_000_000, "MB"
	case quota >= 1_000:
		scale, unit = 1_000, "KB"
	}
	format := func(value int64) string {
		amount := strconv.FormatFloat(float64(value)/float64(scale), 'f', 2, 64)
		return strings.TrimRight(strings.TrimRight(amount, "0"), ".")
	}
	return format(remaining) + " of " + format(quota) + " " + unit
}

func serveSubscription(reg *Registry, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		w.Header().Add("Vary", "Accept")
		token, err := uuid.Parse(r.PathValue("token"))
		if err != nil || token.String() != r.PathValue("token") {
			http.NotFound(w, r)
			return
		}
		u, ok := reg.find(token, true)
		if !ok {
			http.NotFound(w, r)
			return
		}

		profiles := subscriptionProfiles(reg, cfg.ProxyURL, cfg.Reality, u)
		lines := make([]string, 0, len(profiles)+1)
		for _, profile := range profiles {
			lines = append(lines, profile.URI)
		}
		// Clients can display the balance as a profile, but its reserved DNS
		// name and unrelated UUID cannot establish an AtAvix connection.
		lines = append(lines, shareURI(usageOnlyAddress, usageOnlyID, url.Values{
			"encryption": {"none"}, "security": {"none"}, "type": {"tcp"},
		}, subscriptionBalance(u.Remaining(), u.Quota)))
		body := strings.Join(lines, "\n") + "\n"
		w.Header().Set("Subscription-Userinfo", fmt.Sprintf("upload=%d; download=%d; total=%d; expire=0", u.Uploaded, u.Used-u.Uploaded, u.Quota))
		w.Header().Set("Profile-Update-Interval", "1")
		if wantsSubscriptionPage(r) {
			raw := subscriptionURL(cfg.SubscriptionURL, u) + "?format=raw"
			data := subscriptionPage{
				User: u, Profiles: profiles, ShareURL: raw, CopyList: body,
				AndroidLink:         clientImport("v2rayng", base64.RawURLEncoding.EncodeToString([]byte(body))),
				HiddifySubscription: clientImport("hiddify", raw),
				TelegramLink:        template.URL(cfg.Telegram.link(u.ID)),
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := templates.ExecuteTemplate(w, "subscription", data); err != nil {
				slog.Error("Render subscription page", "error", err)
			}
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, base64.StdEncoding.EncodeToString([]byte(body))+"\n")
	}
}

func wantsSubscriptionPage(r *http.Request) bool {
	switch r.URL.Query().Get("format") {
	case "raw":
		return false
	case "html":
		return true
	}
	for _, accept := range r.Header.Values("Accept") {
		for _, alternative := range strings.Split(accept, ",") {
			kind, parameters, err := mime.ParseMediaType(alternative)
			if err != nil || kind != "text/html" {
				continue
			}
			quality, specified := parameters["q"]
			value, err := strconv.ParseFloat(quality, 64)
			if !specified || err == nil && value > 0 && value <= 1 {
				return true
			}
		}
	}
	return false
}
