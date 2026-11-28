package panel

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const nodeHeartbeatPath = "/_atavix/nodes/heartbeat"
const nodeLeasePath = "/_atavix/nodes/lease"
const nodeCredit = 1 << 20
const nodeControlTimeout = 5 * time.Second

type leaseStart struct {
	User uuid.UUID
	Host string
}

type leaseUsage struct {
	Upload, Download int64
	Done             bool
}

type leaseCredit struct {
	Bytes int64
	Stop  bool
}

type nodeLease struct {
	reg     *Registry
	key     [sha256.Size]byte
	state   *userState
	id      uuid.UUID
	stopped atomic.Bool
	credit  int64
	release func()
}

func attachNodeControl(mux *http.ServeMux, reg *Registry) {
	upgrader := websocket.Upgrader{HandshakeTimeout: nodeControlTimeout}
	slots := make(chan struct{}, 4096)
	mux.HandleFunc("POST "+nodeHeartbeatPath, func(w http.ResponseWriter, r *http.Request) {
		key := nodeTokenKey(r)
		var address nodeAddress
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if err := decoder.Decode(&address); err != nil || !address.valid() {
			http.Error(w, "Invalid server address", http.StatusBadRequest)
			return
		}
		reg.mu.Lock()
		idx := reg.nodeIndex(key)
		if idx >= 0 {
			reg.nodes[idx].Address, reg.nodes[idx].Seen = address, time.Now()
		}
		reg.mu.Unlock()
		if idx < 0 {
			http.Error(w, "Unknown server", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+nodeLeasePath, func(w http.ResponseWriter, r *http.Request) {
		key := nodeTokenKey(r)
		reg.mu.Lock()
		idx := reg.nodeIndex(key)
		enabled := idx >= 0 && reg.nodes[idx].Enabled
		reg.mu.Unlock()
		if !enabled {
			http.Error(w, "Server access denied", http.StatusForbidden)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "Service busy", http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadLimit(4096)
		var start leaseStart
		if err := readNodeMessage(conn, &start); err != nil {
			return
		}
		lease := reg.openNodeLease(key, start)
		if lease == nil {
			writeNodeMessage(conn, leaseCredit{Stop: true})
			return
		}
		defer lease.close()
		credit := lease.settle(leaseUsage{})
		for writeNodeMessage(conn, credit) == nil && !credit.Stop {
			var usage leaseUsage
			if err := readNodeMessage(conn, &usage); err != nil {
				return
			}
			credit = lease.settle(usage)
		}
	})
}

func nodeTokenKey(r *http.Request) [sha256.Size]byte {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) != 43 {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256([]byte(token))
}

func readNodeMessage(conn *websocket.Conn, value any) error {
	conn.SetReadDeadline(time.Now().Add(nodeControlTimeout))
	return conn.ReadJSON(value)
}

func writeNodeMessage(conn *websocket.Conn, value any) error {
	conn.SetWriteDeadline(time.Now().Add(nodeControlTimeout))
	return conn.WriteJSON(value)
}

func (r *Registry) openNodeLease(key [sha256.Size]byte, start leaseStart) *nodeLease {
	lease := &nodeLease{reg: r, key: key, id: start.User}
	lease.release = r.connect(start.User, func() { lease.stopped.Store(true) })
	if lease.release == nil {
		return nil
	}
	r.mu.Lock()
	lease.state = r.users[lease.id]
	r.mu.Unlock()
	if lease.state == nil {
		lease.release()
		return nil
	}
	r.observe(start.User, normalizedHostname(start.Host))
	return lease
}

func (l *nodeLease) close() {
	l.reg.mu.Lock()
	l.state.reserved -= l.credit
	l.reg.mu.Unlock()
	l.release()
}

func (l *nodeLease) settle(usage leaseUsage) leaseCredit {
	r := l.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if usage.Upload < 0 || usage.Download < 0 || usage.Upload > l.credit || usage.Download > l.credit-usage.Upload {
		return leaseCredit{Stop: true}
	}
	state := l.state
	state.reserved -= l.credit
	l.credit = 0
	if r.users[l.id] != state {
		return leaseCredit{Stop: true}
	}
	state.user.Used += usage.Upload + usage.Download
	state.user.Uploaded += usage.Upload
	if state.user.Remaining() == 0 {
		state.disconnect()
	}
	idx := r.nodeIndex(l.key)
	if usage.Done || l.stopped.Load() || !state.user.Enabled || idx < 0 || !r.nodes[idx].Enabled {
		return leaseCredit{Stop: true}
	}
	remaining := state.user.Remaining()
	// Leave room for parallel connections when the account is nearly exhausted.
	l.credit = min(nodeCredit, max(relayChunk, remaining/8), max(0, remaining-state.reserved))
	state.reserved += l.credit
	return leaseCredit{Bytes: l.credit}
}
