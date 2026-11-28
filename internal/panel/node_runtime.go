package panel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type nodeClient struct {
	origin *url.URL
	auth   http.Header
	dialer *websocket.Dialer
	http   *http.Client
}

func RunNode(ctx context.Context) error {
	origin, err := origin(os.Getenv("NODE_PANEL_URL"))
	if err != nil || origin.Scheme != "https" {
		return errors.New("NODE_PANEL_URL must be an HTTPS origin")
	}
	tokenFile := env("NODE_TOKEN_FILE", "/etc/atavix-node/token")
	secret, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("read node token: %w", err)
	}
	token := strings.TrimSpace(string(secret))
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 {
		return errors.New("invalid node token")
	}
	reality, err := readReality()
	if err != nil {
		return err
	}
	if reality == nil {
		return errors.New("node requires a public REALITY endpoint")
	}
	privateKey, err := os.ReadFile(env("NODE_REALITY_KEY_FILE", "/etc/atavix-node/reality-key"))
	if err != nil {
		return fmt.Errorf("read node REALITY key: %w", err)
	}
	if err := setNodeIdentity(reality, strings.TrimSpace(string(privateKey)), env("NODE_REALITY_SHORT_ID", "")); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()

	core, err := StartCore(ctx, env("XRAY_BIN", "xray"), reality, listener.Addr().(*net.TCPAddr).Port)
	if err != nil {
		return err
	}
	defer core.Close()
	client := &nodeClient{
		origin: origin, auth: http.Header{"Authorization": {"Bearer " + token}},
		dialer: &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: nodeControlTimeout},
		http:   &http.Client{Timeout: nodeControlTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	address := reality.nodeAddress()
	if err := client.announce(ctx, address); err != nil {
		return err
	}
	gateway := newGateway(core.Socket, core.InternalID, client.authorize)
	defer gateway.Close()
	done := make(chan error, 1)
	go func() { done <- gateway.ServeRaw(listener) }()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	slog.Info("AtAvix node is listening", "reality", address.TCP, "xhttp", address.XHTTP)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-core.Done:
			return core.Err()
		case err := <-done:
			return err
		case <-ticker.C:
			if err := client.announce(ctx, address); err != nil && ctx.Err() == nil {
				slog.Warn("Node heartbeat failed", "error", err)
			}
		}
	}
}

func (c *nodeClient) announce(ctx context.Context, address nodeAddress) error {
	data, err := json.Marshal(address)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.origin.JoinPath(nodeHeartbeatPath).String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header = c.auth.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("panel rejected node heartbeat (HTTP %d)", resp.StatusCode)
	}
	return nil
}

type nodeMeter struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	cancel context.CancelFunc
	credit int64
	usage  leaseUsage
	closed bool
	done   chan struct{}
}

func (c *nodeClient) authorize(ctx context.Context, id uuid.UUID, host string, cancel context.CancelFunc) *tunnelAccount {
	target := c.origin.JoinPath(nodeLeasePath)
	target.Scheme = "wss"
	conn, response, err := c.dialer.DialContext(ctx, target.String(), c.auth)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return nil
	}
	conn.SetReadLimit(2048)
	var initial leaseCredit
	if writeNodeMessage(conn, leaseStart{User: id, Host: host}) != nil ||
		readNodeMessage(conn, &initial) != nil || initial.Stop || initial.Bytes <= 0 || initial.Bytes > nodeCredit {
		conn.Close()
		return nil
	}
	m := &nodeMeter{conn: conn, cancel: cancel, credit: initial.Bytes, done: make(chan struct{})}
	go m.checkpoint()
	return &tunnelAccount{charge: m.charge, exhausted: func() {}, close: m.close}
}

func (m *nodeMeter) charge(n int, upload bool) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credit < int64(n) && !m.exchange(false) {
		return 0, true
	}
	granted := min(m.credit, int64(n))
	m.credit -= granted
	if upload {
		m.usage.Upload += granted
	} else {
		m.usage.Download += granted
	}
	return int(granted), int(granted) < n
}

func (m *nodeMeter) exchange(done bool) bool {
	if m.closed {
		return false
	}
	m.usage.Done = done
	var reply leaseCredit
	if writeNodeMessage(m.conn, m.usage) != nil || readNodeMessage(m.conn, &reply) != nil ||
		reply.Stop || reply.Bytes < 0 || reply.Bytes > nodeCredit {
		m.closed, m.credit = true, 0
		m.conn.Close()
		if !done {
			m.cancel()
		}
		return false
	}
	m.usage = leaseUsage{}
	m.credit = reply.Bytes
	return true
}

func (m *nodeMeter) checkpoint() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			m.mu.Lock()
			ok := m.exchange(false)
			m.mu.Unlock()
			if !ok {
				return
			}
		}
	}
}

func (m *nodeMeter) close() {
	close(m.done)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exchange(true)
	m.conn.Close()
}
