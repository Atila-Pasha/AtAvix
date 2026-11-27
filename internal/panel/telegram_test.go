package panel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/dolonet/mtg-multi/essentials"
	"github.com/google/uuid"
)

func TestTelegramAccountSecretsAndSharedQuota(t *testing.T) {
	ctx := t.Context()
	db := filepath.Join(t.TempDir(), "panel.sqlite")
	reg, err := OpenRegistry(ctx, "sqlite3", db)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	cfg := &Telegram{Public: "proxy.example.org:7445", ServerName: "cover.example.org"}
	if err := reg.initializeTelegram(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	sameEndpoint := &Telegram{Public: cfg.Public, ServerName: cfg.ServerName}
	if err := reg.initializeTelegram(ctx, sameEndpoint); err != nil {
		t.Fatal(err)
	}
	u, err := reg.Save(ctx, uuid.Nil, UserInput{Username: "telegram-user", Remark: "Proxy", Quota: 50, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.link(u.ID) != sameEndpoint.link(u.ID) || cfg.link(u.ID) == cfg.link(uuid.New()) {
		t.Fatal("Telegram credentials need to stay stable and be unique per account")
	}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	peer := &telegramQuotaConn{Conn: essentials.WrapNetConn(server), reader: bufio.NewReader(server), reg: reg, id: u.ID}
	vless, cancel := context.WithCancel(ctx)
	defer cancel()
	releaseVLESS := reg.connect(u.ID, cancel)
	if releaseVLESS == nil {
		t.Fatal("active VLESS account rejected")
	}
	defer releaseVLESS()
	if n, _ := reg.charge(u.ID, 30, true); n != 30 {
		t.Fatal("VLESS traffic rejected")
	}
	go func() { client.Write(make([]byte, 8)) }()
	if n, err := io.ReadFull(peer, make([]byte, 8)); n != 8 || err != nil {
		t.Fatalf("Telegram upload = %d, %v", n, err)
	}
	response := make(chan int, 1)
	go func() {
		n, _ := io.ReadFull(client, make([]byte, 12))
		response <- n
	}()
	if n, err := peer.Write(make([]byte, 20)); n != 12 || err != io.ErrShortWrite {
		t.Fatalf("Telegram download = %d, %v", n, err)
	}
	if n := <-response; n != 12 || vless.Err() == nil || reg.connect(u.ID, func() {}) != nil {
		t.Fatal("Telegram did not close VLESS and reject reconnects on exhaustion")
	}
	updated, _ := reg.find(u.ID, false)
	if updated.Used != 50 || updated.Uploaded != 38 {
		t.Fatalf("shared accounting used %d bytes (%d upstream)", updated.Used, updated.Uploaded)
	}
}

func TestTelegramFakeTLSAdmission(t *testing.T) {
	ctx := t.Context()
	reg, err := OpenRegistry(ctx, "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	cfg := &Telegram{Listen: "127.0.0.1:0", Public: "proxy.example.org:8445", ServerName: "cover.example.org", Cover: "127.0.0.1:1"}
	proxy, err := StartTelegram(ctx, reg, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	u, err := reg.Save(ctx, uuid.Nil, UserInput{Username: "telegram-faketls", Remark: "Telegram", Quota: 1_000_000, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secret := cfg.secret(u.ID)
	hello := testTelegramHello(t, secret.Host, secret.Key[:])
	// Fragment the Random field between TLS records to exercise normalization.
	fragment := func(payload []byte) []byte {
		return append([]byte{22, 3, 1, byte(len(payload) >> 8), byte(len(payload))}, payload...)
	}
	wire := append(fragment(hello[5:23]), fragment(hello[23:])...)
	normalized, n := telegramHello(bufio.NewReader(bytes.NewReader(wire)))
	if n != len(wire) || !bytes.Equal(normalized, hello) {
		t.Fatal("fragmented ClientHello failed normalization")
	}
	key := telegramKey{id: u.ID, key: secret.Key}
	if matchTelegramHello(normalized, []telegramKey{key}, time.Now().Add(time.Minute)) != uuid.Nil {
		t.Fatal("expired ClientHello was accepted")
	}

	conn, err := net.DialTimeout("tcp", proxy.listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	var response []byte
	for _, kind := range []byte{22, 20, 23} {
		record := testReadTelegramRecord(t, conn)
		if record[0] != kind {
			t.Fatalf("expected TLS record %d, got %d", kind, record[0])
		}
		response = append(response, record...)
	}
	got := append([]byte(nil), response[11:43]...)
	clear(response[11:43])
	mac := hmac.New(sha256.New, secret.Key[:])
	mac.Write(hello[11:43])
	mac.Write(response)
	if !hmac.Equal(got, mac.Sum(nil)) {
		t.Fatal("invalid FakeTLS server authentication")
	}
	updated, _ := reg.find(u.ID, false)
	if updated.Used != int64(len(response)) || updated.Uploaded != 0 {
		t.Fatalf("Telegram handshake accounting used %d bytes (%d upstream)", updated.Used, updated.Uploaded)
	}
}

func testTelegramHello(t *testing.T, host string, key []byte) []byte {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	server.SetReadDeadline(time.Now().Add(time.Second))
	go tls.Client(client, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS13}).Handshake()
	record := testReadTelegramRecord(t, server)
	clear(record[11:43])
	mac := hmac.New(sha256.New, key)
	mac.Write(record)
	copy(record[11:43], mac.Sum(nil))
	stamp := binary.LittleEndian.Uint32(record[39:43]) ^ uint32(time.Now().Unix())
	binary.LittleEndian.PutUint32(record[39:43], stamp)
	return record
}

func testReadTelegramRecord(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, binary.BigEndian.Uint16(header[3:]))
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatal(err)
	}
	return append(header, payload...)
}
