package panel

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/dolonet/mtg-multi/antireplay"
	"github.com/dolonet/mtg-multi/essentials"
	"github.com/dolonet/mtg-multi/events"
	"github.com/dolonet/mtg-multi/ipblocklist"
	"github.com/dolonet/mtg-multi/logger"
	"github.com/dolonet/mtg-multi/mtglib"
	mtgnetwork "github.com/dolonet/mtg-multi/network/v2"
	"github.com/google/uuid"
)

const (
	telegramHelloLimit   = 16 << 10
	telegramClockSkew    = 30 * time.Second
	telegramReadTimeout  = 8 * time.Second
	telegramConcurrent   = 512
	telegramRandomOffset = 11
	telegramRandomSize   = 32
)

type Telegram struct {
	Public, Listen, ServerName, Cover string
	seed                              [32]byte
}

func readTelegram() (*Telegram, error) {
	public := env("TELEGRAM_PUBLIC", "")
	if public == "" {
		return nil, nil
	}
	listen := env("TELEGRAM_LISTEN", "0.0.0.0:8445")
	if _, err := parseRealityEndpoint(public, listen); err != nil {
		return nil, fmt.Errorf("TELEGRAM_PUBLIC / TELEGRAM_LISTEN: %w", err)
	}
	sni := normalizedHostname(env("TELEGRAM_SERVER_NAME", ""))
	if sni == "" {
		return nil, errors.New("TELEGRAM_SERVER_NAME must be the public TLS cover hostname")
	}
	cover := env("TELEGRAM_COVER", net.JoinHostPort(sni, "443"))
	coverHost, coverPort, err := splitEndpoint(cover)
	if err != nil || (net.ParseIP(coverHost) == nil && normalizedHostname(coverHost) == "" && coverHost != "caddy") {
		return nil, errors.New("TELEGRAM_COVER must contain a TLS cover address and port")
	}
	_, listenPort, _ := splitEndpoint(listen)
	if coverPort == listenPort {
		return nil, errors.New("TELEGRAM_COVER and TELEGRAM_LISTEN must use separate TCP ports")
	}
	return &Telegram{Public: public, Listen: listen, ServerName: sni, Cover: cover}, nil
}

func (r *Registry) initializeTelegram(ctx context.Context, cfg *Telegram) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS telegram_identity (
		id INTEGER PRIMARY KEY CHECK (id = 1), seed TEXT NOT NULL
	)`); err != nil {
		return err
	}
	const query = "SELECT seed FROM telegram_identity WHERE id=1"
	var encoded string
	err := r.db.QueryRowContext(ctx, query).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		var seed [32]byte
		rand.Read(seed[:])
		if _, err := r.db.ExecContext(ctx, `INSERT INTO telegram_identity (id, seed)
			VALUES (1, $1) ON CONFLICT (id) DO NOTHING`, base64.RawStdEncoding.EncodeToString(seed[:])); err != nil {
			return err
		}
		err = r.db.QueryRowContext(ctx, query).Scan(&encoded)
	}
	if err != nil {
		return err
	}
	seed, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(seed) != len(cfg.seed) {
		return errors.New("stored Telegram identity is invalid")
	}
	copy(cfg.seed[:], seed)
	return nil
}

func (cfg *Telegram) secret(id uuid.UUID) mtglib.Secret {
	mac := hmac.New(sha256.New, cfg.seed[:])
	mac.Write(id[:])
	secret := mtglib.Secret{Host: cfg.ServerName}
	copy(secret.Key[:], mac.Sum(nil))
	return secret
}

func (cfg *Telegram) link(id uuid.UUID) string {
	if cfg == nil {
		return ""
	}
	host, port, _ := splitEndpoint(cfg.Public)
	proxy := url.URL{Scheme: "tg", Host: "proxy", RawQuery: url.Values{
		"server": {host}, "port": {strconv.Itoa(port)}, "secret": {cfg.secret(id).Hex()},
	}.Encode()}
	return proxy.String()
}

type TelegramServer struct {
	cfg      *Telegram
	reg      *Registry
	proxy    *mtglib.Proxy
	listener net.Listener
	replays  mtglib.AntiReplayCache
	done     chan error
	cancel   context.CancelFunc
	finished chan struct{}
	mu       sync.Mutex
	revision uint64
	keys     []telegramKey
}

type telegramKey struct {
	id  uuid.UUID
	key [mtglib.SecretKeyLength]byte
}

// StartTelegram serves the FakeTLS MTProto protocol and shares the VLESS quota registry.
func StartTelegram(ctx context.Context, reg *Registry, cfg *Telegram) (*TelegramServer, error) {
	if err := reg.initializeTelegram(ctx, cfg); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	coverHost, coverPort, _ := splitEndpoint(cfg.Cover)
	network := mtgnetwork.New(nil, "", 5*time.Second, 10*time.Second, 30*time.Second, net.KeepAliveConfig{}, 0)
	proxy, err := mtglib.NewProxy(mtglib.ProxyOpts{
		// The engine requires a nonempty set even when the panel has zero users.
		Secret: cfg.secret(uuid.Nil), Network: network,
		AntiReplayCache: antireplay.NewStableBloomFilter(1<<20, 1e-8),
		IPBlocklist:     ipblocklist.NewNoop(), IPAllowlist: ipblocklist.NewNoop(),
		EventStream: events.NewNoopStream(), Logger: logger.NewNoopLogger(),
		DomainFrontingHost: coverHost, DomainFrontingPort: uint(coverPort),
		TolerateTimeSkewness: telegramClockSkew, HandshakeTimeout: telegramReadTimeout,
		PreferIP: "prefer-ipv4", DCPoolEnabled: true,
	})
	if err != nil {
		listener.Close()
		return nil, err
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	s := &TelegramServer{
		cfg: cfg, reg: reg, proxy: proxy, listener: listener, cancel: cancel,
		replays: antireplay.NewStableBloomFilter(1<<20, 1e-8),
		done:    make(chan error, 1), finished: make(chan struct{}),
	}
	go s.serve(serveCtx)
	return s, nil
}

func (s *TelegramServer) Done() <-chan error { return s.done }

func (s *TelegramServer) Close() {
	s.cancel()
	s.listener.Close()
	<-s.finished
}

func (s *TelegramServer) serve(ctx context.Context) {
	defer close(s.finished)
	defer s.proxy.Shutdown()
	var running sync.WaitGroup
	defer running.Wait()
	slots := make(chan struct{}, telegramConcurrent)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				s.done <- err
				s.cancel()
			}
			return
		}
		select {
		case slots <- struct{}{}:
			running.Go(func() {
				defer func() { <-slots }()
				s.handle(ctx, conn.(*net.TCPConn))
			})
		default:
			conn.Close()
		}
	}
}

func (s *TelegramServer) accountKeys() ([]telegramKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg.mu.Lock()
	revision := s.reg.accountRevision
	if s.revision == revision {
		s.reg.mu.Unlock()
		return s.keys, nil
	}
	ids := make([]uuid.UUID, 0, len(s.reg.users))
	for id := range s.reg.users {
		ids = append(ids, id)
	}
	s.reg.mu.Unlock()

	secrets := map[string]mtglib.Secret{"reserved": s.cfg.secret(uuid.Nil)}
	keys := make([]telegramKey, 0, len(ids))
	for _, id := range ids {
		secret := s.cfg.secret(id)
		keys = append(keys, telegramKey{id, secret.Key})
		secrets[id.String()] = secret
	}
	if _, err := s.proxy.UpdateSecrets(secrets); err != nil {
		return nil, err
	}
	s.revision, s.keys = revision, keys
	return keys, nil
}

func (s *TelegramServer) handle(ctx context.Context, client *net.TCPConn) {
	defer client.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	client.SetDeadline(time.Now().Add(telegramReadTimeout))
	reader := bufio.NewReaderSize(client, telegramHelloLimit)
	hello, wireSize := telegramHello(reader)
	if wireSize == 0 {
		return
	}
	keys, err := s.accountKeys()
	if err != nil {
		return
	}
	id := matchTelegramHello(hello, keys, time.Now())
	if id == uuid.Nil || s.replays.SeenBefore(hello[telegramRandomOffset:telegramRandomOffset+telegramRandomSize]) {
		s.cover(ctx, client, reader)
		return
	}
	release := s.reg.connect(id, cancel)
	if release == nil {
		return
	}
	defer release()
	s.proxy.ServeConn(&telegramQuotaConn{Conn: client, reader: reader, helloBytes: wireSize, id: id, reg: s.reg})
}

func (s *TelegramServer) cover(ctx context.Context, client *net.TCPConn, reader *bufio.Reader) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", s.cfg.Cover)
	if err != nil {
		return
	}
	defer upstream.Close()
	upstream.SetDeadline(time.Now().Add(telegramReadTimeout))
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		io.Copy(upstream, reader)
		upstream.(*net.TCPConn).CloseWrite()
	}()
	io.Copy(client, upstream)
	client.SetReadDeadline(time.Now())
	<-finished
}

// FakeTLS authenticates the ClientHello with its random field zeroed. Merge
// handshake fragments into one canonical TLS record, as the MTProto engine does.
func telegramHello(reader *bufio.Reader) ([]byte, int) {
	var handshake []byte
	for offset, fragments := 0, 0; fragments < 10; fragments++ {
		header, err := reader.Peek(offset + 5)
		if err != nil || header[offset] != 22 || header[offset+1] != 3 || header[offset+2] != 1 {
			return nil, 0
		}
		size := int(binary.BigEndian.Uint16(header[offset+3:]))
		end := offset + 5 + size
		record, err := reader.Peek(end)
		if err != nil {
			return nil, 0
		}
		handshake = append(handshake, record[offset+5:end]...)
		offset = end
		if len(handshake) < 4 {
			continue
		}
		length := 4 + (int(handshake[1]) << 16) + (int(handshake[2]) << 8) + int(handshake[3])
		if handshake[0] != 1 || length < 38 || length > telegramHelloLimit-5 {
			return nil, 0
		}
		if len(handshake) >= length {
			hello := []byte{22, 3, 1, byte(length >> 8), byte(length)}
			return append(hello, handshake[:length]...), offset
		}
	}
	return nil, 0
}

func matchTelegramHello(hello []byte, keys []telegramKey, now time.Time) uuid.UUID {
	var zero [telegramRandomSize]byte
	random := hello[telegramRandomOffset : telegramRandomOffset+telegramRandomSize]
	for _, account := range keys {
		mac := hmac.New(sha256.New, account.key[:])
		mac.Write(hello[:telegramRandomOffset])
		mac.Write(zero[:])
		mac.Write(hello[telegramRandomOffset+telegramRandomSize:])
		digest := mac.Sum(nil)
		if subtle.ConstantTimeCompare(digest[:28], random[:28]) != 1 {
			continue
		}
		stamp := binary.LittleEndian.Uint32(digest[28:]) ^ binary.LittleEndian.Uint32(random[28:])
		if now.Sub(time.Unix(int64(stamp), 0)).Abs() <= telegramClockSkew {
			return account.id
		}
	}
	return uuid.Nil
}

type telegramQuotaConn struct {
	essentials.Conn
	reader     *bufio.Reader
	helloBytes int
	reg        *Registry
	id         uuid.UUID
}

func (c *telegramQuotaConn) Read(buf []byte) (int, error) {
	if c.helloBytes > 0 {
		n, err := c.reader.Read(buf[:min(len(buf), c.helloBytes)])
		c.helloBytes -= n
		return n, err
	}
	n, err := c.reader.Read(buf)
	if n == 0 {
		return n, err
	}
	allowed, exhausted := c.reg.charge(c.id, n, true)
	if exhausted {
		c.reg.stopExhausted(c.id)
	}
	if allowed < n || exhausted {
		return allowed, io.EOF
	}
	return allowed, err
}

func (c *telegramQuotaConn) Write(buf []byte) (int, error) {
	allowed, exhausted := c.reg.charge(c.id, len(buf), false)
	var n int
	var err error
	if allowed > 0 {
		n, err = c.Conn.Write(buf[:allowed])
	}
	if exhausted {
		c.reg.stopExhausted(c.id)
	}
	if err == nil && allowed < len(buf) {
		err = io.ErrShortWrite
	}
	return n, err
}
