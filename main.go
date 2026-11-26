package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Atila-Pasha/AtAvix/internal/panel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("AtAvix stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := panel.ReadConfig()
	if err != nil {
		return err
	}
	startup, done := context.WithTimeout(ctx, 20*time.Second)
	defer done()
	reg, err := panel.OpenRegistry(startup, cfg.Driver, cfg.Database)
	if err != nil {
		return err
	}
	defer reg.Close()

	core, err := panel.StartCore(context.Background(), cfg.CoreBinary)
	if err != nil {
		return err
	}
	defer core.Close()
	gateway := panel.NewGateway(reg, core.Socket, core.InternalID)
	defer gateway.Close()
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	checkpointCtx, cancelCheckpoints := context.WithCancel(context.Background())
	defer cancelCheckpoints()
	checkpointsDone := make(chan struct{})
	go func() {
		defer close(checkpointsDone)
		reg.Checkpoint(checkpointCtx)
	}()
	server := &http.Server{
		Handler: panel.NewHandler(cfg, reg, gateway), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	slog.Info("AtAvix is listening", "address", listener.Addr(), "panel", cfg.PanelURL)

	select {
	case <-ctx.Done():
	case <-core.Done:
		err = core.Err()
	case err = <-served:
	}
	gateway.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if shutdownErr := server.Shutdown(shutdown); shutdownErr != nil {
		server.Close()
		err = errors.Join(err, shutdownErr)
	}
	cancelCheckpoints()
	<-checkpointsDone
	return errors.Join(err, reg.Flush(shutdown))
}
