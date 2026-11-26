package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

type Core struct {
	Socket     string
	InternalID uuid.UUID
	Done       <-chan struct{}
	cancel     context.CancelFunc
	dir        string
	err        error
}

func StartCore(ctx context.Context, binary string, reality *Reality, forwardPort int) (*Core, error) {
	dir, err := os.MkdirTemp("", "atavix-")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	exited := make(chan struct{})
	core := &Core{Socket: filepath.Join(dir, "xray.sock"), InternalID: uuid.New(), Done: exited, cancel: cancel, dir: dir}

	var ingress string
	if reality != nil {
		data, err := json.Marshal(map[string]any{
			"listen": reality.listenHost, "port": reality.listenPort, "protocol": "tunnel",
			"settings": map[string]any{"allowedNetwork": "tcp", "rewriteAddress": "127.0.0.1", "rewritePort": forwardPort},
			"streamSettings": map[string]any{
				"network": "raw", "security": "reality",
				"realitySettings": map[string]any{
					"target": reality.Target, "serverNames": []string{reality.ServerName},
					"privateKey": reality.privateKey, "shortIds": []string{reality.shortID},
				},
			},
		})
		if err != nil {
			cancel()
			os.RemoveAll(dir)
			return nil, err
		}
		ingress = "," + string(data)
	}
	config := fmt.Sprintf(xrayConfig, core.Socket, core.InternalID.String(), ingress)
	path := filepath.Join(dir, "xray.json")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}

	var output bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "run", "-config", path)
	if reality != nil {
		// The tunnel inbound must keep REALITY encryption on its downlink.
		cmd.Env = append(os.Environ(), "xray.buf.splice=disable")
	}
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		cancel()
		os.RemoveAll(dir)
		return nil, err
	}
	go func() {
		err := cmd.Wait()
		if ctx.Err() == nil {
			core.err = fmt.Errorf("Xray exited: %v: %s", err, output.String())
		}
		close(exited)
	}()

	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-exited:
			core.Close()
			return nil, fmt.Errorf("starting Xray: %v", core.err)
		case <-deadline.C:
			core.Close()
			return nil, fmt.Errorf("Xray socket did not open: %s", output.String())
		case <-ticker.C:
			conn, err := net.DialTimeout("unix", core.Socket, 50*time.Millisecond)
			if err == nil {
				conn.Close()
				return core, nil
			}
		}
	}
}

func (c *Core) Err() error {
	<-c.Done
	return c.err
}

func (c *Core) Close() {
	c.cancel()
	<-c.Done
	os.RemoveAll(c.dir)
}

const xrayConfig = `{
  "log": {"access": "none", "error": "none", "loglevel": "none"},
  "inbounds": [{
    "listen": %q,
    "protocol": "vless",
    "settings": {"decryption": "none", "clients": [{"id": %q}]}
  }%s],
  "outbounds": [{
    "protocol": "freedom",
    "settings": {"finalRules": [{"action": "block", "network": "tcp", "port": "25,465,587", "blockDelay": "0"}]},
    "streamSettings": {"sockopt": {"domainStrategy": "ForceIP"}}
  }],
  "policy": {"levels": {"0": {"handshake": 8, "connIdle": 120, "uplinkOnly": 4, "downlinkOnly": 5}}}
}`
