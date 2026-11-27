package panel

import (
	"encoding/base64"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSubscriptionPageAndClientImport(t *testing.T) {
	reg, err := OpenRegistry(t.Context(), "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	u, err := reg.Save(t.Context(), uuid.Nil, UserInput{
		Username: "mobile-user", Remark: "North & West", Quota: 20_000_000_000, Enabled: true, TrackDomains: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://sub.example.org:2053")
	reg.charge(u.ID, 5_000_000_000, true)
	reg.charge(u.ID, 12_400_000_000, false)
	const privateDomain = "private-activity.example.org"
	reg.observe(u.ID, privateDomain)
	if err := reg.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if domains, err := reg.Activity(t.Context(), u.ID); err != nil || len(domains) != 1 || domains[0].Hostname != privateDomain {
		t.Fatal("expected a recorded domain for the privacy check")
	}
	reality := &Reality{
		realityEndpoint: realityEndpoint{Public: "vpn.example.org:7443"},
		XHTTP:           realityEndpoint{Public: "vpn.example.org:7444"}, ServerName: "sub.example.org",
	}
	if err := reg.InitializeReality(t.Context(), reality); err != nil {
		t.Fatal(err)
	}
	server := NewHandler(Config{PanelURL: base, SubscriptionURL: base, ProxyURL: base, Reality: reality}, reg, nil)
	get := func(path, accept string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, base.String()+path, nil)
		r.Header.Set("Accept", accept)
		server.ServeHTTP(w, r)
		return w
	}

	path := "/" + u.Token.String()
	browser := get(path, "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	body := browser.Body.String()
	if browser.Code != http.StatusOK || !strings.HasPrefix(browser.Header().Get("Content-Type"), "text/html") ||
		browser.Header().Get("Cache-Control") != "no-store" || browser.Header().Get("Vary") != "Accept" ||
		browser.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unexpected browser response headers: %d %v", browser.Code, browser.Header())
	}
	for _, fragment := range []string{"North &amp; West (MCI)", "North &amp; West (Irancell)", "North &amp; West (Rightel)", "Data remaining", "/assets/subscription.css", `class="brand-mark"><img src="/assets/atavix-emblem.webp"`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("subscription page missing %q", fragment)
		}
	}
	for _, fragment := range []string{
		"Usage statistics", "17.4 GB", "2.6 GB", "20 GB",
		`<span>Download</span><strong>12.4 GB</strong>`, `class="download-bar" value="12400000000" max="17400000000"`,
		`<span>Upload</span><strong>5 GB</strong>`, `class="upload-bar" value="5000000000" max="17400000000"`,
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("usage charts missing %q", fragment)
		}
	}
	if strings.Contains(body, privateDomain) {
		t.Fatal("subscription disclosed a recorded domain")
	}
	android := regexp.MustCompile(`href="(v2rayng:[^"]+)"`).FindStringSubmatch(body)
	if len(android) != 2 {
		t.Fatal("missing Android import action")
	}
	action, err := url.Parse(html.UnescapeString(android[1]))
	if err != nil || action.Host != "install-config" {
		t.Fatalf("invalid Android import action: %v", err)
	}

	feed := get(path, "*/*")
	explicitFeed := get(path+"?format=raw", "text/html")
	rejectedHTML := get(path, "text/html;q=0,*/*")
	if !strings.HasPrefix(feed.Header().Get("Content-Type"), "text/plain") ||
		feed.Body.String() != explicitFeed.Body.String() || feed.Body.String() != rejectedHTML.Body.String() {
		t.Fatal("client requests did not return the same subscription feed")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(feed.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(decoded), privateDomain) {
		t.Fatal("subscription feed disclosed a recorded domain")
	}
	batch, err := base64.RawURLEncoding.DecodeString(action.Query().Get("url"))
	if err != nil || string(batch) != string(decoded) {
		t.Fatal("Android action must import the exact profile list")
	}
	if !strings.Contains(body, "hiddify://install-config?url="+url.QueryEscape(base.String()+path+"?format=raw")) {
		t.Fatal("Hiddify subscription action must receive the raw subscription URL")
	}
	lines := strings.Split(strings.TrimSpace(string(decoded)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected three connection profiles and an informational row, got %d", len(lines))
	}
	for i, expected := range []string{"reality", "tls", "reality", "none"} {
		profile, err := url.Parse(lines[i])
		if err != nil || profile.Query().Get("security") != expected {
			t.Fatalf("unexpected transport security at row %d: %v", i, err)
		}
	}
	balance, err := url.Parse(lines[3])
	if err != nil || balance.Fragment != "2.6 of 20 GB" || balance.Host != usageOnlyAddress {
		t.Fatal("invalid display-only quota line")
	}
	xhttp, err := url.Parse(lines[2])
	if err != nil || xhttp.Query().Get("type") != "xhttp" || xhttp.Query().Get("path") != xhttpPath {
		t.Fatal("expected a distinct XHTTP profile")
	}
	if _, ok := reg.find(usageOnlyID, false); ok {
		t.Fatal("usage-only UUID resolves to a real account")
	}
	oldBalance := get("/balance", "*/*")
	if oldBalance.Code != http.StatusNotFound {
		t.Fatalf("the retired balance listener returned %d", oldBalance.Code)
	}
	if asset := get("/assets/subscription.css", "text/css"); asset.Code != http.StatusOK {
		t.Fatalf("subscription stylesheet returned %d", asset.Code)
	}
	if asset := get("/assets/atavix-emblem.webp", "image/webp"); asset.Code != http.StatusOK || asset.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("brand emblem returned %d with content type %q", asset.Code, asset.Header().Get("Content-Type"))
	}
}
