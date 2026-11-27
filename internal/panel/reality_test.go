package panel

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestXrayReality(t *testing.T) {
	t.Run("TCP", func(t *testing.T) { testXrayReality(t, false) })
	t.Run("XHTTP", func(t *testing.T) { testXrayReality(t, true) })
}

func testXrayReality(t *testing.T, xhttp bool) {
	binary := os.Getenv("XRAY_TEST_BINARY")
	if binary == "" {
		t.Skip("set XRAY_TEST_BINARY to exercise the REALITY listener")
	}
	ctx := t.Context()
	reg, err := OpenRegistry(ctx, "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	target.EnableHTTP2 = true
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	target.StartTLS()
	defer target.Close()

	raw, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	port := availablePort(t)
	reality := &Reality{
		realityEndpoint: realityEndpoint{Public: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), listenHost: "127.0.0.1", listenPort: port},
		ServerName:      "example.com", Target: target.Listener.Addr().String(),
	}
	endpoint := reality.realityEndpoint
	if xhttp {
		port = availablePort(t)
		reality.XHTTP = realityEndpoint{Public: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), listenHost: "127.0.0.1", listenPort: port}
		endpoint = reality.XHTTP
	}
	if err := reg.InitializeReality(ctx, reality); err != nil {
		t.Fatal(err)
	}
	second := &Reality{}
	if err := reg.InitializeReality(ctx, second); err != nil || second.publicKey != reality.publicKey || second.shortID != reality.shortID {
		t.Fatalf("REALITY identity was not retained: %v", err)
	}
	input := UserInput{Username: "reality-user", Remark: "Direct + Test", Enabled: true, Quota: 1_000_000, TrackDomains: true}
	account, err := reg.Save(ctx, uuid.Nil, input)
	if err != nil {
		t.Fatal(err)
	}
	core, err := StartCore(ctx, binary, reality, raw.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	gateway := NewGateway(reg, core.Socket, core.InternalID)
	defer gateway.Close()
	go gateway.ServeRaw(raw)

	uri, err := url.Parse(reality.profileAt(endpoint.Public, account.ID, input.Remark, xhttp))
	if err != nil || uri.Fragment != input.Remark || strings.Contains(uri.String(), reality.privateKey) {
		t.Fatal("invalid REALITY share URI or disclosure of private key")
	}
	query := uri.Query()
	proxyPort := availablePort(t)
	config := map[string]any{
		"log":      map[string]string{"loglevel": "error"},
		"inbounds": []any{map[string]any{"listen": "127.0.0.1", "port": proxyPort, "protocol": "http"}},
		"outbounds": []any{map[string]any{
			"protocol": "vless",
			"settings": map[string]any{"vnext": []any{map[string]any{
				"address": uri.Hostname(), "port": port,
				"users": []any{map[string]string{"id": uri.User.Username(), "encryption": query.Get("encryption")}},
			}}},
			"streamSettings": map[string]any{
				"network": query.Get("type"), "security": query.Get("security"),
				"realitySettings": map[string]string{
					"serverName": query.Get("sni"), "password": query.Get("pbk"), "shortId": query.Get("sid"),
					"fingerprint": query.Get("fp"), "spiderX": query.Get("spx"),
				},
			},
		}},
	}
	if xhttp {
		stream := config["outbounds"].([]any)[0].(map[string]any)["streamSettings"].(map[string]any)
		stream["xhttpSettings"] = map[string]string{"path": query.Get("path"), "mode": query.Get("mode")}
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(t.TempDir(), "client.json")
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, binary, "run", "-config", configFile)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		command.Process.Kill()
		command.Wait()
	}()
	proxyAddress := net.JoinHostPort("127.0.0.1", strconv.Itoa(proxyPort))
	for deadline := time.Now().Add(5 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", proxyAddress, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Xray HTTP client proxy did not start")
		}
		time.Sleep(25 * time.Millisecond)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: proxyAddress}), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err := client.Get("https://example.com/")
	if err != nil {
		t.Fatalf("REALITY connection failed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("proxied HTTP status: %s", response.Status)
	}
	if err := reg.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	domains, err := reg.Activity(ctx, account.ID)
	used, _ := reg.find(account.ID, false)
	if err != nil || len(domains) != 1 || domains[0].Hostname != "example.com" || used.Used == 0 {
		t.Fatalf("REALITY accounting: usage %d, domains %v, error %v", used.Used, domains, err)
	}

	input.Quota = used.Used
	if _, err := reg.Save(ctx, account.ID, input); err != nil {
		t.Fatal(err)
	}
	response, err = client.Get("https://example.com/")
	if err == nil {
		response.Body.Close()
		t.Fatal("REALITY accepted an exhausted account")
	}
}

func availablePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
