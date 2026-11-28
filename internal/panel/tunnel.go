package panel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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
	PrimaryPath             = "/connect"
	AlternatePath           = "/fallback"
	websocketEarlyDataLimit = 2048
	relayChunk              = 64 << 10
)

var hostnameLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var relayBuffers = sync.Pool{New: func() any { return new([relayChunk]byte) }}

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

type Gateway struct {
	authorize  func(context.Context, uuid.UUID, string, context.CancelFunc) *tunnelAccount
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
	return newGateway(socket, internalID, func(_ context.Context, id uuid.UUID, host string, cancel context.CancelFunc) *tunnelAccount {
		release := reg.connect(id, cancel)
		if release == nil {
			return nil
		}
		return &tunnelAccount{
			charge:    func(n int, upload bool) (int, bool) { return reg.charge(id, n, upload) },
			exhausted: func() { reg.stopExhausted(id) },
			close:     release,
			observe:   func() { reg.observe(id, host) },
		}
	})
}

func newGateway(socket string, internalID uuid.UUID, authorize func(context.Context, uuid.UUID, string, context.CancelFunc) *tunnelAccount) *Gateway {
	ctx, cancel := context.WithCancel(context.Background())
	return &Gateway{
		authorize: authorize, socket: socket, internalID: internalID,
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
	if !websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	var earlyData []byte
	var responseHeader http.Header
	if protocols := r.Header.Values("Sec-WebSocket-Protocol"); len(protocols) != 0 {
		if len(protocols) != 1 || len(protocols[0]) > base64.RawURLEncoding.EncodedLen(websocketEarlyDataLimit) {
			http.NotFound(w, r)
			return
		}
		var err error
		earlyData, err = base64.RawURLEncoding.Strict().DecodeString(protocols[0])
		if err != nil || len(earlyData) == 0 || earlyData[0] != 0 {
			http.NotFound(w, r)
			return
		}
		responseHeader = http.Header{"Sec-WebSocket-Protocol": {protocols[0]}}
	}
	if !g.reserve() {
		http.Error(w, "Service busy", http.StatusServiceUnavailable)
		return
	}
	defer g.release()
	client, err := g.upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	client.SetReadLimit(8 << 20)
	stream := &wsStream{Conn: client}
	if len(earlyData) != 0 {
		stream.frame = bytes.NewReader(earlyData)
	}
	g.tunnel(stream)
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

type tunnelAccount struct {
	charge    func(int, bool) (int, bool)
	exhausted func()
	close     func()
	observe   func()
}

func (g *Gateway) tunnel(client tunnelStream) {
	defer client.Close()
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	stopClient := context.AfterFunc(ctx, func() { client.Close() })
	defer stopClient()

	client.SetReadDeadline(time.Now().Add(10 * time.Second))
	input := bufio.NewReader(client)
	header, id, host, err := readVLESSHeader(input)
	if err != nil {
		return
	}
	account := g.authorize(ctx, id, host, cancel)
	if account == nil {
		return
	}
	defer account.close()
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
	if !account.send(core, header, true) {
		return
	}
	if account.observe != nil {
		account.observe()
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		account.relay(core, client, false)
		cancel()
	}()
	if err := account.relay(input, core, true); err == io.EOF {
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

func (a *tunnelAccount) relay(source io.Reader, target tunnelStream, upload bool) error {
	buffer := relayBuffers.Get().(*[relayChunk]byte)
	defer relayBuffers.Put(buffer)
	for {
		n, err := source.Read(buffer[:])
		if n > 0 && !a.send(target, buffer[:n], upload) {
			return io.ErrClosedPipe
		}
		if err != nil {
			return err
		}
	}
}

func (a *tunnelAccount) send(target tunnelStream, payload []byte, upload bool) bool {
	allowed, exhausted := a.charge(len(payload), upload)
	if allowed == 0 {
		return false
	}
	target.SetWriteDeadline(time.Now().Add(15 * time.Second))
	_, err := target.Write(payload[:allowed])
	if exhausted {
		a.exhausted()
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
