package panel

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

const maxNodes = 32
const nodeOnlineFor = 45 * time.Second

var errNodeInput = errors.New("Enter a server name of 1–40 characters and a two-letter country code.")
var errNodeName = errors.New("A server with that name already exists.")
var errNodeLimit = errors.New("The server limit has been reached.")

type Node struct {
	ID      uuid.UUID
	Name    string
	Country string
	Enabled bool
	Address nodeAddress
	Seen    time.Time
	key     [sha256.Size]byte
}

type nodeAddress struct {
	TCP, XHTTP                     string
	ServerName, PublicKey, ShortID string
}

func (a nodeAddress) valid() bool {
	for _, endpoint := range []string{a.TCP, a.XHTTP} {
		if endpoint == "" && a.TCP != "" {
			continue
		}
		host, _, err := splitEndpoint(endpoint)
		if err != nil || (net.ParseIP(host) == nil && normalizedHostname(host) == "") {
			return false
		}
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(a.PublicKey)
	shortID, shortErr := hex.DecodeString(a.ShortID)
	return err == nil && len(key) == 32 && shortErr == nil && len(shortID) == 8 &&
		a.TCP != a.XHTTP && normalizedHostname(a.ServerName) == a.ServerName && a.ServerName != ""
}

func countryCode(value string) (string, bool) {
	code := strings.ToUpper(strings.TrimSpace(value))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return "", false
	}
	region, err := language.ParseRegion(code)
	return code, err == nil && region.IsCountry()
}

func countryLabel(code string) string {
	region, _ := language.ParseRegion(code)
	return string([]rune{'🇦' + rune(code[0]-'A'), '🇦' + rune(code[1]-'A')}) + " " + display.English.Regions().Name(region)
}

func (n Node) Location() string { return countryLabel(n.Country) }

func (n Node) Status() string {
	switch {
	case !n.Enabled:
		return "Disabled"
	case time.Since(n.Seen) < nodeOnlineFor:
		return "Online"
	default:
		return "Offline"
	}
}

func (r *Registry) loadNodes(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS proxy_nodes (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, name_key TEXT NOT NULL UNIQUE,
		country TEXT NOT NULL, enabled BOOLEAN NOT NULL, token_hash TEXT NOT NULL UNIQUE
	)`); err != nil {
		return err
	}
	rows, err := r.db.QueryContext(ctx, "SELECT id, name, country, enabled, token_hash FROM proxy_nodes ORDER BY name_key")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var node Node
		var hash string
		if err := rows.Scan(&node.ID, &node.Name, &node.Country, &node.Enabled, &hash); err != nil {
			return err
		}
		bytes, err := hex.DecodeString(hash)
		if err != nil || len(bytes) != len(node.key) {
			return errors.New("invalid stored node token hash")
		}
		copy(node.key[:], bytes)
		r.nodes = append(r.nodes, node)
	}
	return rows.Err()
}

func (r *Registry) Nodes() []Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Node(nil), r.nodes...)
}

func (r *Registry) saveNode(ctx context.Context, id uuid.UUID, name, country string, enabled bool) (string, error) {
	name = strings.TrimSpace(name)
	code, valid := countryCode(country)
	if !valid || name == "" || utf8.RuneCountInString(name) > 40 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errNodeInput
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, node := range r.nodes {
		if node.ID != id && strings.EqualFold(node.Name, name) {
			return "", errNodeName
		}
	}
	for i, node := range r.nodes {
		if node.ID == id {
			if _, err := r.db.ExecContext(ctx, "UPDATE proxy_nodes SET name=$1, name_key=$2, country=$3, enabled=$4 WHERE id=$5",
				name, strings.ToLower(name), code, enabled, id); err != nil {
				return "", err
			}
			r.nodes[i].Name, r.nodes[i].Country, r.nodes[i].Enabled = name, code, enabled
			return "", nil
		}
	}
	if id != uuid.Nil {
		return "", ErrMissing
	}
	if len(r.nodes) >= maxNodes {
		return "", errNodeLimit
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	node := Node{ID: uuid.New(), Name: name, Country: code, Enabled: enabled, key: sha256.Sum256([]byte(token))}
	if _, err := r.db.ExecContext(ctx, "INSERT INTO proxy_nodes (id,name,name_key,country,enabled,token_hash) VALUES ($1,$2,$3,$4,$5,$6)",
		node.ID, name, strings.ToLower(name), code, enabled, hex.EncodeToString(node.key[:])); err != nil {
		return "", err
	}
	r.nodes = append(r.nodes, node)
	return token, nil
}

func (r *Registry) deleteNode(ctx context.Context, id uuid.UUID) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, node := range r.nodes {
		if node.ID == id {
			if _, err := r.db.ExecContext(ctx, "DELETE FROM proxy_nodes WHERE id=$1", id); err != nil {
				return err
			}
			r.nodes = append(r.nodes[:i], r.nodes[i+1:]...)
			return nil
		}
	}
	return ErrMissing
}

func (r *Registry) nodeIndex(key [sha256.Size]byte) int {
	for i, node := range r.nodes {
		if node.key == key {
			return i
		}
	}
	return -1
}

func (r *Registry) nodeProfiles(u User) []connectionProfile {
	var profiles []connectionProfile
	for _, node := range r.Nodes() {
		if node.Status() != "Online" {
			continue
		}
		a := node.Address
		reality := Reality{ServerName: a.ServerName, publicKey: a.PublicKey, shortID: a.ShortID}
		name := fmt.Sprintf("%s · %s · %s", node.Location(), node.Name, u.Remark)
		profiles = append(profiles, connectionProfile{name + " (TCP)", "TCP + REALITY", reality.profileAt(a.TCP, u.ID, name+" (TCP)", false)})
		if a.XHTTP != "" {
			profiles = append(profiles, connectionProfile{name + " (XHTTP)", "XHTTP + REALITY", reality.profileAt(a.XHTTP, u.ID, name+" (XHTTP)", true)})
		}
	}
	return profiles
}

func (r *Reality) nodeAddress() nodeAddress {
	return nodeAddress{r.Public, r.XHTTP.Public, r.ServerName, r.publicKey, r.shortID}
}

func setNodeIdentity(r *Reality, privateKey, shortID string) error {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(privateKey)
	if err != nil {
		return errors.New("invalid node REALITY private key")
	}
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		return errors.New("invalid node REALITY private key")
	}
	id, err := hex.DecodeString(shortID)
	if err != nil || len(id) != 8 {
		return errors.New("node REALITY short ID must be 16 hexadecimal digits")
	}
	r.privateKey, r.publicKey, r.shortID = privateKey, base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), shortID
	return nil
}
