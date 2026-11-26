package panel

import (
	"crypto/rand"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestGatewaySharesBandwidthAcrossTransports(t *testing.T) {
	reg, err := OpenRegistry(t.Context(), "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	account, err := reg.Save(t.Context(), uuid.Nil, UserInput{Username: "speed-test", Remark: "Speed test", Enabled: true, Quota: 10_000_000})
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
	streams := []tunnelStream{tcp, &wsStream{Conn: ws}}
	packet := append([]byte{0}, account.ID[:]...)
	packet = append(packet, 0, 1, 1, 187, 1, 192, 0, 2, 1)
	headerSize := len(packet)
	const payloadSize = 600_000
	packet = append(packet, make([]byte, payloadSize)...)
	rand.Read(packet[headerSize:])

	start := time.Now()
	results := make(chan error, len(streams)*2)
	for _, stream := range streams {
		stream.SetReadDeadline(start.Add(8 * time.Second))
		stream.SetWriteDeadline(start.Add(8 * time.Second))
		go func() {
			_, err := stream.Write(packet)
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
	elapsed := time.Since(start)
	minimum := time.Duration(want-bandwidthBurst) * time.Second / bytesPerSec
	if elapsed < minimum-10*time.Millisecond {
		t.Fatalf("traffic exceeded the account rate: %d bytes in %v, expected at least %v", want, elapsed, minimum)
	}
	latest, _ := reg.find(account.ID, false)
	if latest.Used != want {
		t.Fatalf("shared quota accounted for %d bytes, want %d", latest.Used, want)
	}
	t.Logf("shared rate %.2f Mbps across both transports", float64(want)*8/elapsed.Seconds()/1e6)
}
