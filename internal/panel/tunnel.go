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
)

var hostnameLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func subscriptionURL(base *url.URL, u User) string {
	return base.JoinPath(u.Token.String()).String()
}

func profileAddress(base *url.URL) string {
	port := base.Port()
	if port == "" {
		port = "80"
		if base.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(base.Hostname(), port)
}

func profile(base *url.URL, u User, path, name string) string {
	query := url.Values{"encryption": {"none"}, "security": {"none"}, "type": {"ws"}, "path": {path}, "host": {base.Host}}
	if base.Scheme == "https" {
		query.Set("security", "tls")
		query.Set("sni", base.Hostname())
	}
	return shareURI(profileAddress(base), u.ID, query, name)
}

func shareURI(address string, id uuid.UUID, query url.Values, name string) string {
	link := url.URL{Scheme: "vless", User: url.User(id.String()), Host: address, RawQuery: query.Encode()}
	return link.String() + "#" + strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}

func serveSubscription(reg *Registry, base *url.URL, reality *Reality) http.HandlerFunc {
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
		body := strings.Join(subscriptionProfiles(reg, base, reality, u), "\n") + "\n"
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Subscription-Userinfo", fmt.Sprintf("upload=%d; download=%d; total=%d; expire=0", u.Uploaded, u.Used-u.Uploaded, u.Quota))
		w.Header().Set("Profile-Update-Interval", "1")
		io.WriteString(w, base64.StdEncoding.EncodeToString([]byte(body))+"\n")
	}
}

type Gateway struct {
	reg        *Registry
	internalID uuid.UUID
	socket     string
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
		reg: reg, socket: socket, internalID: internalID,
		upgrader: websocket.Upgrader{HandshakeTimeout: 5 * time.Second},
		ctx:      ctx, cancel: cancel, slots: make(chan struct{}, 1024),
	}
}

func (g *Gateway) Close() {
	g.mu.Lock()
	g.cancel()
	g.mu.Unlock()
	g.running.Wait()
}

func (g *Gateway) reserve() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() != nil {
		return false
	}
	select {
	case g.slots <- struct{}{}:
		g.running.Add(1)
		return true
	default:
		return false
	}
}

func (g *Gateway) release() {
	<-g.slots
	g.running.Done()
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) || r.Header.Get("Sec-WebSocket-Protocol") != "" {
		http.NotFound(w, r)
		return
	}
	if !g.reserve() {
		http.Error(w, "Service busy", http.StatusServiceUnavailable)
		return
	}
	defer g.release()
	client, err := g.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client.SetReadLimit(8 << 20)
	g.tunnel(&wsStream{Conn: client})
}

// The raw listener accepts streams from Xray after it terminates REALITY.
func (g *Gateway) ServeRaw(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		if !g.reserve() {
			conn.Close()
			continue
		}
		go func() {
			defer g.release()
			g.tunnel(conn)
		}()
	}
}

type tunnelStream interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

func (g *Gateway) tunnel(client tunnelStream) {
	defer client.Close()
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	stopClient := context.AfterFunc(ctx, func() { client.Close() })
	defer stopClient()

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	header, id, host, err := readVLESSHeader(client)
	if err != nil {
		return
	}
	limit, release := g.reg.connect(id, cancel)
	if limit == nil {
		return
	}
	defer release()
	client.SetReadDeadline(time.Time{})
	dialer := net.Dialer{Timeout: 5 * time.Second}
	core, err := dialer.DialContext(ctx, "unix", g.socket)
	if err != nil {
		return
	}
	defer core.Close()
	stopCore := context.AfterFunc(ctx, func() { core.Close() })
	defer stopCore()
	copy(header[1:17], g.internalID[:])
	if !g.send(ctx, limit, id, core, header, true) {
		return
	}
	g.reg.observe(id, host)

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		g.relay(ctx, limit, id, core, client, false)
		cancel()
	}()
	if err := g.relay(ctx, limit, id, client, core, true); err == io.EOF {
		// A TCP client may finish sending and still expect a reply.
		if _, raw := client.(net.Conn); raw {
			core.(*net.UnixConn).CloseWrite()
			<-finished
			return
		}
	}
	cancel()
	<-finished
}

func (g *Gateway) relay(ctx context.Context, limit *bandwidth, id uuid.UUID, source io.Reader, target tunnelStream, upload bool) error {
	buffer := make([]byte, relayChunk)
	for {
		n, err := source.Read(buffer)
		if n > 0 && !g.send(ctx, limit, id, target, buffer[:n], upload) {
			return io.ErrClosedPipe
		}
		if err != nil {
			return err
		}
	}
}

func (g *Gateway) send(ctx context.Context, limit *bandwidth, id uuid.UUID, target tunnelStream, payload []byte, upload bool) bool {
	if err := limit.wait(ctx, len(payload)); err != nil {
		return false
	}
	allowed, exhausted := g.reg.charge(id, len(payload), upload)
	if allowed == 0 {
		return false
	}
	target.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := target.Write(payload[:allowed])
	if exhausted {
		g.reg.stopExhausted(id)
	}
	return err == nil && !exhausted
}

type wsStream struct {
	*websocket.Conn
	frame io.Reader
}

func (r *wsStream) Read(p []byte) (int, error) {
	for {
		if r.frame == nil {
			kind, frame, err := r.NextReader()
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

func (r *wsStream) Write(payload []byte) (int, error) {
	if err := r.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
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
