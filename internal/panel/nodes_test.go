package panel

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

func TestNodeEnrollmentAndUsage(t *testing.T) {
	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "panel.sqlite")
	reg, err := OpenRegistry(ctx, "sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	token, err := reg.saveNode(ctx, uuid.Nil, "Istanbul", "tr", true)
	if err != nil {
		t.Fatal(err)
	}
	node := reg.Nodes()[0]
	if node.Status() != "Offline" {
		t.Fatal("a new server was published before its heartbeat")
	}
	input := UserInput{Username: "nodes-test", Remark: "Shared quota", Quota: 20_000_000, Enabled: true, TrackDomains: true}
	user, err := reg.Save(ctx, uuid.Nil, input)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	attachNodeControl(mux, reg)
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	base, _ := url.Parse(server.URL)
	client := &nodeClient{
		origin: base, http: server.Client(),
		dialer: &websocket.Dialer{TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig},
		auth:   http.Header{"Authorization": {"Bearer " + token}},
	}
	address := nodeAddress{
		TCP: "198.51.100.18:7443", XHTTP: "198.51.100.18:7444", ServerName: "example.com",
		PublicKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), ShortID: "abcdef1234567890",
	}
	if err := client.announce(ctx, address); err != nil {
		t.Fatal(err)
	}
	profiles := reg.nodeProfiles(user)
	if len(profiles) != 2 || !strings.HasPrefix(profiles[0].Name, "🇹🇷 ") {
		t.Fatalf("expected two flagged profiles, got %v", profiles)
	}
	for _, profile := range profiles {
		link, err := url.Parse(profile.URI)
		if err != nil || link.Fragment != profile.Name || link.User.Username() != user.ID.String() || link.Hostname() != "198.51.100.18" {
			t.Fatal("incorrect node URI")
		}
	}
	tunnel, cancel := context.WithCancel(ctx)
	defer cancel()
	account := client.authorize(tunnel, user.ID, "www.example.com", cancel)
	if account == nil {
		t.Fatal("authorized account was refused")
	}
	if n, _ := account.charge(123, true); n != 123 {
		t.Fatal("the node could not use its upload credit")
	}
	if n, _ := account.charge(456, false); n != 456 {
		t.Fatal("the node could not use its download credit")
	}
	account.close()
	latest, _ := reg.find(user.ID, false)
	if latest.Used != 579 || latest.Uploaded != 123 {
		t.Fatalf("incorrect node accounting: %d %d", latest.Used, latest.Uploaded)
	}
	if n, _ := reg.charge(user.ID, 10, true); n != 10 {
		t.Fatal("local connection failed to share the quota")
	}
	tunnel, stop := context.WithCancel(ctx)
	defer stop()
	account = client.authorize(tunnel, user.ID, "", stop)
	if account == nil {
		t.Fatal("expected a live connection")
	}
	defer account.close()
	input.Enabled = false
	if _, err := reg.Save(ctx, user.ID, input); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tunnel.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("pausing a user did not stop the remote tunnel")
	}
	if account := client.authorize(ctx, user.ID, "", func() {}); account != nil {
		account.close()
		t.Fatal("paused account was accepted")
	}
	if _, err := reg.saveNode(ctx, node.ID, "Istanbul", "TR", false); err != nil {
		t.Fatal(err)
	}
	if len(reg.nodeProfiles(user)) != 0 {
		t.Fatal("a disabled node is still advertised")
	}
	if err := reg.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenRegistry(ctx, "sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if nodes := reloaded.Nodes(); len(nodes) != 1 || nodes[0].Enabled || nodes[0].Country != "TR" {
		t.Fatal("node settings were not retained")
	}
	if saved, _ := reloaded.find(user.ID, false); saved.Used != 589 {
		t.Fatalf("accounting was not retained: %d", saved.Used)
	}
}

func TestNodeReservationsDoNotDoubleSpend(t *testing.T) {
	ctx := t.Context()
	reg, err := OpenRegistry(ctx, "sqlite3", filepath.Join(t.TempDir(), "panel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	for _, name := range []string{"Frankfurt", "Istanbul"} {
		if _, err := reg.saveNode(ctx, uuid.Nil, name, "DE", true); err != nil {
			t.Fatal(err)
		}
	}
	nodes := reg.Nodes()
	user, err := reg.Save(ctx, uuid.Nil, UserInput{Username: "credit-test", Remark: "Test", Quota: 200_000, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	first := reg.openNodeLease(nodes[0].key, leaseStart{User: user.ID})
	second := reg.openNodeLease(nodes[1].key, leaseStart{User: user.ID})
	if first == nil || second == nil {
		t.Fatal("expected two remote connections")
	}
	defer first.close()
	defer second.close()
	a, b := first.settle(leaseUsage{}).Bytes, second.settle(leaseUsage{}).Bytes
	local, _ := reg.charge(user.ID, 200_000, true)
	if a <= 0 || b <= 0 || a+b+int64(local) != user.Quota {
		t.Fatal("reservations exceeded or stranded the available quota")
	}
	if grant := first.settle(leaseUsage{Upload: a, Done: true}); !grant.Stop {
		t.Fatal("closing connection should receive no further credit")
	}
	second.settle(leaseUsage{Download: b, Done: true})
	latest, _ := reg.find(user.ID, false)
	if latest.Used != user.Quota || latest.Uploaded != a+int64(local) || latest.Remaining() != 0 {
		t.Fatal("quota did not reconcile across local and remote sessions")
	}
}
