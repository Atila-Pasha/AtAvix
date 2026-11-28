package panel

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestGatewaySharesQuotaAcrossTransports(t *testing.T) {
	reg, err := OpenRegistry(t.Context(), "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	account, err := reg.Save(t.Context(), uuid.Nil, UserInput{Username: "quota-test", Remark: "Quota test", Enabled: true, Quota: 10_000_000})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := net.Listen("unix", filepath.Join(t.TempDir(), "echo.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if _, _, _, err := readVLESSHeader(conn); err == nil {
					io.Copy(conn, conn)
				}
			}()
		}
	}()

	gateway := NewGateway(reg, upstream.Addr().String(), uuid.New())
	defer gateway.Close()
	server := httptest.NewServer(gateway)
	defer server.Close()
	raw, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	go gateway.ServeRaw(raw)

	tcp, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+PrimaryPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	packet := append([]byte{0}, account.ID[:]...)
	packet = append(packet, 0, 1, 1, 187, 1, 192, 0, 2, 1)
	headerSize := len(packet)
	const payloadSize = 600_000
	packet = append(packet, make([]byte, payloadSize)...)
	rand.Read(packet[headerSize:])

	const earlyBytes = 512
	protocol := base64.RawURLEncoding.EncodeToString(packet[:earlyBytes])
	earlyDialer := websocket.Dialer{Subprotocols: []string{protocol}}
	early, _, err := earlyDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+AlternatePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	if early.Subprotocol() != protocol {
		t.Fatal("WebSocket upgrade did not acknowledge early data")
	}

	streams := []struct {
		tunnelStream
		upload []byte
	}{
		{tcp, packet},
		{&wsStream{Conn: ws}, packet},
		{&wsStream{Conn: early}, packet[earlyBytes:]},
	}
	start := time.Now()
	results := make(chan error, len(streams)*2)
	for _, stream := range streams {
		stream.SetReadDeadline(start.Add(8 * time.Second))
		stream.SetWriteDeadline(start.Add(8 * time.Second))
		go func() {
			_, err := stream.Write(stream.upload)
			results <- err
		}()
		go func() {
			_, err := io.CopyN(io.Discard, stream, payloadSize)
			results <- err
		}()
	}
	for range len(streams) * 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	want := int64(len(streams) * (payloadSize*2 + headerSize))
	latest, _ := reg.find(account.ID, false)
	if latest.Used != want {
		t.Fatalf("shared quota accounted for %d bytes, want %d", latest.Used, want)
	}
}

func TestGatewayRejectsInvalidWebSocketEarlyData(t *testing.T) {
	gateway := NewGateway(nil, "", uuid.Nil)
	defer gateway.Close()
	server := httptest.NewServer(gateway)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + PrimaryPath
	for _, protocols := range [][]string{
		{"not-vless"},
		{base64.RawURLEncoding.EncodeToString(make([]byte, websocketEarlyDataLimit+1))},
		{"AAA", "AAA"},
	} {
		dialer := websocket.Dialer{Subprotocols: protocols}
		conn, response, err := dialer.Dial(endpoint, nil)
		if conn != nil {
			conn.Close()
		}
		if response != nil {
			response.Body.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
			t.Fatalf("expected HTTP 404 for malformed early data, got %v", err)
		}
	}
}
