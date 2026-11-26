package panel

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/google/uuid"
)

type Reality struct {
	Public, ServerName, Target string
	listenHost                 string
	listenPort                 int
	privateKey, publicKey      string
	shortID                    string
}

func readReality() (*Reality, error) {
	public := env("REALITY_PUBLIC", "")
	if public == "" {
		return nil, nil
	}
	host, _, err := splitEndpoint(public)
	if err != nil || (net.ParseIP(host) == nil && normalizedHostname(host) == "") {
		return nil, errors.New("REALITY_PUBLIC must contain a public hostname or IP and TCP port")
	}
	sni := normalizedHostname(env("REALITY_SERVER_NAME", ""))
	if sni == "" {
		return nil, errors.New("REALITY_SERVER_NAME must be the TLS hostname of the REALITY target")
	}
	target := env("REALITY_TARGET", net.JoinHostPort(sni, "443"))
	if _, _, err := splitEndpoint(target); err != nil {
		return nil, fmt.Errorf("REALITY_TARGET: %w", err)
	}
	listenHost, listenPort, err := splitEndpoint(env("REALITY_LISTEN", "0.0.0.0:8443"))
	if err != nil || net.ParseIP(listenHost) == nil {
		return nil, errors.New("REALITY_LISTEN must contain a listen IP and TCP port")
	}
	return &Reality{Public: public, ServerName: sni, Target: target, listenHost: listenHost, listenPort: listenPort}, nil
}

func splitEndpoint(s string) (string, int, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" {
		return "", 0, errors.New("enter a host and TCP port")
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", 0, errors.New("TCP port must be between 1 and 65535")
	}
	return host, int(number), nil
}

func (r *Registry) InitializeReality(ctx context.Context, transport *Reality) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS reality_identity (
		id INTEGER PRIMARY KEY CHECK (id = 1), private_key TEXT NOT NULL, short_id TEXT NOT NULL
	)`); err != nil {
		return err
	}
	var privateKey, shortID string
	const query = "SELECT private_key, short_id FROM reality_identity WHERE id=1"
	err := r.db.QueryRowContext(ctx, query).Scan(&privateKey, &shortID)
	if errors.Is(err, sql.ErrNoRows) {
		key, generateErr := ecdh.X25519().GenerateKey(rand.Reader)
		if generateErr != nil {
			return generateErr
		}
		var freshShortID [8]byte
		if _, err := rand.Read(freshShortID[:]); err != nil {
			return err
		}
		if _, err := r.db.ExecContext(ctx, `INSERT INTO reality_identity (id, private_key, short_id)
			VALUES (1, $1, $2) ON CONFLICT (id) DO NOTHING`, base64.RawURLEncoding.EncodeToString(key.Bytes()), hex.EncodeToString(freshShortID[:])); err != nil {
			return err
		}
		err = r.db.QueryRowContext(ctx, query).Scan(&privateKey, &shortID)
	}
	if err != nil {
		return err
	}
	keyBytes, err := base64.RawURLEncoding.DecodeString(privateKey)
	if err != nil {
		return errors.New("stored REALITY private key is invalid")
	}
	key, err := ecdh.X25519().NewPrivateKey(keyBytes)
	if err != nil {
		return errors.New("stored REALITY private key is invalid")
	}
	transport.privateKey, transport.shortID = privateKey, shortID
	transport.publicKey = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	return nil
}

func (r *Reality) profileAt(address string, id uuid.UUID, name string) string {
	query := url.Values{
		"encryption": {"none"}, "security": {"reality"}, "type": {"tcp"},
		"sni": {r.ServerName}, "fp": {"chrome"}, "pbk": {r.publicKey}, "sid": {r.shortID}, "spx": {"/"},
	}
	return shareURI(address, id, query, name)
}
