package panel

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	PrimaryPath = "/connect"
	BalancePath = "/balance"
	corePath    = "/internal"
)

var hostnameLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func subscriptionURL(base *url.URL, u User) string {
	return base.JoinPath(u.Token.String()).String()
}

func profile(base *url.URL, u User, path, name string) string {
	security := "none"
	port := base.Port()
	if base.Scheme == "https" {
		security = "tls"
		if port == "" {
			port = "443"
		}
	} else if port == "" {
		port = "80"
	}
	query := url.Values{"encryption": {"none"}, "security": {security}, "type": {"ws"}, "path": {path}, "host": {base.Host}}
	if security == "tls" {
		query.Set("sni", base.Hostname())
	}
	link := url.URL{Scheme: "vless", User: url.User(u.ID.String()), Host: net.JoinHostPort(base.Hostname(), port), RawQuery: query.Encode()}
	return link.String() + "#" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

func serveSubscription(reg *Registry, base *url.URL) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
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
		body := profile(base, u, PrimaryPath, u.Remark) + "\n" +
			profile(base, u, BalancePath, u.Username+"-"+FormatBytes(u.Remaining())) + "\n"
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Subscription-Userinfo", fmt.Sprintf("upload=%d; download=%d; total=%d; expire=0", u.Uploaded, u.Used-u.Uploaded, u.Quota))
		w.Header().Set("Profile-Update-Interval", "1")
		io.WriteString(w, base64.StdEncoding.EncodeToString([]byte(body))+"\n")
	}
}

type Gateway struct {
	reg        *Registry
	internalID uuid.UUID
	upstream   websocket.Dialer
	upgrader   websocket.Upgrader
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	running    sync.WaitGroup
	slots      chan struct{}
}

func NewGateway(reg *Registry, socket string, internalID uuid.UUID) *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	return &Gateway{
		reg: reg, internalID: internalID,
		upstream: websocket.Dialer{
			HandshakeTimeout: 5 * time.Second,
			NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
		ctx: ctx, cancel: cancel, slots: make(chan struct{}, 1024),
	}
}

func (g *Gateway) Close() {
	g.mu.Lock()
	g.cancel()
	g.mu.Unlock()
	g.running.Wait()
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) || r.Header.Get("Sec-WebSocket-Protocol") != "" {
		http.NotFound(w, r)
		return
	}
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case g.slots <- struct{}{}:
	default:
		g.mu.Unlock()
		http.Error(w, "Service busy", http.StatusServiceUnavailable)
		return
	}
	g.running.Add(1)
	g.mu.Unlock()
	defer g.running.Done()
	defer func() { <-g.slots }()

	client, err := g.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	stopClient := context.AfterFunc(ctx, func() { client.Close() })
	defer stopClient()

	client.SetReadLimit(8 << 20)
	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	in := &wsReader{conn: client}
	header, id, host, err := readVLESSHeader(in)
	if err != nil {
		return
	}
	release, ok := g.reg.connect(id, cancel)
	if !ok {
		return
	}
	defer release()
	client.SetReadDeadline(time.Time{})
	core, response, err := g.upstream.DialContext(ctx, "ws://localhost"+corePath, nil)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return
	}
	defer core.Close()
	stopCore := context.AfterFunc(ctx, func() { core.Close() })
	defer stopCore()
	core.SetReadLimit(8 << 20)
	copy(header[1:17], g.internalID[:])
	if !g.send(id, core, header, true) {
		return
	}
	g.reg.observe(id, host)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		g.relay(id, &wsReader{conn: core}, client, false)
		cancel()
	}()
	g.relay(id, in, core, true)
	cancel()
	<-finished
}

func (g *Gateway) relay(id uuid.UUID, source io.Reader, target *websocket.Conn, upload bool) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := source.Read(buffer)
		if n > 0 && !g.send(id, target, buffer[:n], upload) {
			return
		}
		if err != nil {
			return
		}
	}
}

func (g *Gateway) send(id uuid.UUID, target *websocket.Conn, payload []byte, upload bool) bool {
	allowed, exhausted := g.reg.charge(id, len(payload), upload)
	if allowed == 0 {
		return false
	}
	target.SetWriteDeadline(time.Now().Add(15 * time.Second))
	err := target.WriteMessage(websocket.BinaryMessage, payload[:allowed])
	if exhausted {
		g.reg.stopExhausted(id)
	}
	return err == nil && !exhausted
}

type wsReader struct {
	conn  *websocket.Conn
	frame io.Reader
}

func (r *wsReader) Read(p []byte) (int, error) {
	for {
		if r.frame == nil {
			kind, frame, err := r.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				return 0, errors.New("expected a binary WebSocket message")
			}
			r.frame = frame
		}
		n, err := r.frame.Read(p)
		if err == io.EOF {
			r.frame = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

// Read only the VLESS request header. The rest of the tunnel remains opaque.
func readVLESSHeader(source io.Reader) ([]byte, uuid.UUID, string, error) {
	var header bytes.Buffer
	input := io.TeeReader(source, &header)
	var initial [18]byte
	if _, err := io.ReadFull(input, initial[:]); err != nil {
		return nil, uuid.Nil, "", err
	}
	id := uuid.UUID(initial[1:17])
	if initial[0] != 0 {
		return nil, uuid.Nil, "", errors.New("unsupported VLESS version")
	}
	var addons [255]byte
	if _, err := io.ReadFull(input, addons[:initial[17]]); err != nil {
		return nil, uuid.Nil, "", err
	}
	var command [1]byte
	if _, err := io.ReadFull(input, command[:]); err != nil {
		return nil, uuid.Nil, "", err
	}
	if command[0] == 3 {
		return header.Bytes(), id, "", nil
	}
	if command[0] != 1 && command[0] != 2 {
		return nil, uuid.Nil, "", errors.New("unsupported VLESS command")
	}
	var destination [3]byte
	if _, err := io.ReadFull(input, destination[:]); err != nil {
		return nil, uuid.Nil, "", err
	}
	var address [255]byte
	var length int
	switch destination[2] {
	case 1:
		length = 4
	case 2:
		if _, err := io.ReadFull(input, address[:1]); err != nil {
			return nil, uuid.Nil, "", err
		}
		length = int(address[0])
	case 3:
		length = 16
	default:
		return nil, uuid.Nil, "", errors.New("unsupported VLESS destination type")
	}
	if _, err := io.ReadFull(input, address[:length]); err != nil {
		return nil, uuid.Nil, "", err
	}
	host := ""
	if destination[2] == 2 {
		host = normalizedHostname(string(address[:length]))
	}
	return header.Bytes(), id, host, nil
}

func normalizedHostname(value string) string {
	host := strings.ToLower(strings.TrimSuffix(value, "."))
	if len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return ""
	}
	for label := range strings.SplitSeq(host, ".") {
		if !hostnameLabel.MatchString(label) {
			return ""
		}
	}
	return host
}
