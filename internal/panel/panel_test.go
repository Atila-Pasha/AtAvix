package panel

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestSubscriptionAndAccounting(t *testing.T) {
	ctx := context.Background()
	driver, dsn := "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite")
	if postgres := os.Getenv("TEST_POSTGRES_URL"); postgres != "" {
		driver, dsn = "pgx", postgres
	}
	reg, err := OpenRegistry(ctx, driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	name := "member-" + uuid.NewString()[:8]
	input := UserInput{Username: name, Remark: "Studio + North", Quota: 10_000_000_000, Enabled: true, TrackDomains: true}
	u, err := reg.Save(ctx, uuid.Nil, input)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, err := OpenRegistry(ctx, driver, dsn)
		if err == nil {
			defer clean.Close()
			clean.Delete(ctx, u.ID)
		}
	})
	if u.ID == u.Token || u.ID == uuid.Nil || u.Token == uuid.Nil {
		t.Fatal("expected distinct UUIDs")
	}
	if _, err := reg.Save(ctx, uuid.Nil, input); err != ErrUsername {
		t.Fatalf("duplicate username: %v", err)
	}
	base, _ := url.Parse("https://sub.example.org")
	mux := http.NewServeMux()
	mux.Handle("GET /{token}", serveSubscription(reg, base))
	read := func(key uuid.UUID) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/"+key.String(), nil))
		return w
	}
	if w := read(u.ID); w.Code != http.StatusNotFound {
		t.Fatalf("proxy UUID accessed the subscription: %d", w.Code)
	}
	checkProfiles := func(expectedBalance, expectedHeader string) {
		t.Helper()
		w := read(u.Token)
		if w.Code != http.StatusOK || w.Header().Get("Subscription-Userinfo") != expectedHeader {
			t.Fatalf("unexpected subscription response: %d, %v", w.Code, w.Header())
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(w.Body.String()))
		if err != nil {
			t.Fatal(err)
		}
		profiles := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(profiles) != 2 {
			t.Fatalf("got %d profiles", len(profiles))
		}
		for i, expectedPath := range []string{PrimaryPath, BalancePath} {
			q, err := url.Parse(profiles[i])
			if err != nil || q.Scheme != "vless" || q.Host != "sub.example.org:443" ||
				q.User.Username() != u.ID.String() || q.Query().Get("path") != expectedPath ||
				q.Query().Get("security") != "tls" || q.Query().Get("sni") != "sub.example.org" {
				t.Fatalf("invalid profile %d: %s", i, profiles[i])
			}
			fragment := []string{u.Remark, u.Username + "-" + expectedBalance}[i]
			if q.Fragment != fragment {
				t.Fatalf("unexpected remark %q", q.Fragment)
			}
		}
	}
	checkProfiles("10 GB", "upload=0; download=0; total=10000000000; expire=0")
	reg.charge(u.ID, 1_000_000_000, true)
	reg.charge(u.ID, 1_500_000_000, false)
	checkProfiles("7.5 GB", "upload=1000000000; download=1500000000; total=10000000000; expire=0")
	reg.observe(u.ID, "example.com")
	if err := reg.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenRegistry(ctx, driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	loaded, ok := reloaded.find(u.Token, true)
	if !ok || loaded.Used != 2_500_000_000 || loaded.Uploaded != 1_000_000_000 {
		t.Fatalf("usage did not survive reload: %+v", loaded)
	}
	domains, err := reg.Activity(ctx, u.ID)
	if err != nil || len(domains) != 1 || domains[0].Hostname != "example.com" {
		t.Fatalf("domain activity: %v, %v", domains, err)
	}
	input.TrackDomains = false
	input.Quota = 2_500_000_002
	if _, err := reg.Save(ctx, u.ID, input); err != nil {
		t.Fatal(err)
	}
	if domains, err := reg.Activity(ctx, u.ID); err != nil || len(domains) != 0 {
		t.Fatalf("disabling tracking kept activity: %v, %v", domains, err)
	}
	stopped := false
	limit, release := reg.connect(u.ID, func() { stopped = true })
	if limit == nil {
		t.Fatal("could not open a tunnel with quota remaining")
	}
	defer release()
	second, releaseSecond := reg.connect(u.ID, func() {})
	if second != limit {
		t.Fatal("concurrent tunnels did not share the same bandwidth bucket")
	}
	defer releaseSecond()
	now := time.Now()
	if limit.acquire(relayChunk, now) != 0 || second.acquire(relayChunk, now) != 0 {
		t.Fatal("initial burst was unexpectedly shaped")
	}
	expected := time.Duration(relayChunk * int64(time.Second) / bytesPerSec)
	if delay := second.acquire(relayChunk, now); delay != expected {
		t.Fatalf("bandwidth delay: got %v, want %v", delay, expected)
	}
	granted, exhausted := reg.charge(u.ID, 5, false)
	reg.stopExhausted(u.ID)
	if granted != 2 || !exhausted || !stopped {
		t.Fatalf("quota did not terminate the tunnel: %d %v %v", granted, exhausted, stopped)
	}
	if limit, _ := reg.connect(u.ID, func() {}); limit != nil {
		t.Fatal("exhausted account was allowed to connect")
	}
}

func TestXrayTunnel(t *testing.T) {
	binary := os.Getenv("XRAY_TEST_BINARY")
	if binary == "" {
		t.Skip("set XRAY_TEST_BINARY to run the Xray integration test against example.com")
	}
	ctx := context.Background()
	reg, err := OpenRegistry(ctx, "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	u, err := reg.Save(ctx, uuid.Nil, UserInput{Username: "test-member", Remark: "Test", Enabled: true, Quota: 1_000_000, TrackDomains: true})
	if err != nil {
		t.Fatal(err)
	}
	core, err := StartCore(ctx, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	gateway := NewGateway(reg, core.Socket, core.InternalID)
	defer gateway.Close()
	server := httptest.NewServer(gateway)
	defer server.Close()
	request := []byte{0}
	request = append(request, u.ID[:]...)
	request = append(request, 0, 1, 1, 187, 2, 11)
	request = append(request, "example.com"...)
	for _, path := range []string{PrimaryPath, BalancePath} {
		t.Run(path, func(t *testing.T) {
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			client.SetReadDeadline(time.Now().Add(12 * time.Second))
			client.SetWriteDeadline(time.Now().Add(12 * time.Second))
			if err := client.WriteMessage(websocket.BinaryMessage, request[:10]); err != nil {
				t.Fatal(err)
			}
			if err := client.WriteMessage(websocket.BinaryMessage, request[10:]); err != nil {
				t.Fatal(err)
			}
			transport := &testTransport{Conn: client.UnderlyingConn(), socket: client, reader: wsReader{conn: client}, header: true}
			conn := tls.Client(transport, &tls.Config{ServerName: "example.com", MinVersion: tls.VersionTLS12})
			defer conn.Close()
			if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("proxied HTTP status: %s", response.Status)
			}
		})
	}
	if err := reg.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	domains, err := reg.Activity(ctx, u.ID)
	current, _ := reg.find(u.ID, false)
	if err != nil || len(domains) != 1 || domains[0].Hostname != "example.com" ||
		current.Uploaded == 0 || current.Used <= current.Uploaded {
		t.Fatalf("unexpected traffic or domain accounting: %d/%d, %v, %v", current.Uploaded, current.Used, domains, err)
	}
}

type testTransport struct {
	net.Conn
	socket *websocket.Conn
	reader wsReader
	header bool
}

func (c *testTransport) Read(p []byte) (int, error) {
	if c.header {
		c.header = false
		var head [2]byte
		if _, err := io.ReadFull(&c.reader, head[:]); err != nil {
			return 0, err
		}
		if head[0] != 0 {
			return 0, fmt.Errorf("invalid VLESS response")
		}
		if _, err := io.CopyN(io.Discard, &c.reader, int64(head[1])); err != nil {
			return 0, err
		}
	}
	return c.reader.Read(p)
}

func (c *testTransport) Write(p []byte) (int, error) {
	if err := c.socket.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
