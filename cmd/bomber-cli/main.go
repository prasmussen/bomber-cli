package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	ssh "github.com/charmbracelet/ssh"

	"bomber-cli/internal/host"
)

func main() {
	if err := run(parseConfig()); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func parseConfig() host.Config {
	cfg := host.DefaultConfig()
	flag.StringVar(&cfg.Listen, "listen", cfg.Listen, "TCP listen address")
	flag.StringVar(&cfg.HostKey, "host-key", cfg.HostKey, "persistent Ed25519 SSH host key")
	flag.IntVar(&cfg.MaxSessions, "max-sessions", cfg.MaxSessions, "maximum connected sessions")
	flag.IntVar(&cfg.MaxRooms, "max-rooms", cfg.MaxRooms, "maximum rooms")
	flag.Parse()
	return cfg
}

func run(cfg host.Config) error {
	h, err := host.New(cfg)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		_ = shutdownHost(h)
		return err
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- h.Serve(listener) }()
	slog.Info("listening", "address", listener.Addr(), "host_key", cfg.HostKey)
	select {
	case <-signals.Done():
	case err = <-done:
		if errors.Is(err, ssh.ErrServerClosed) {
			err = nil
		}
	}
	closeErr := shutdownHost(h)
	if err != nil {
		return err
	}
	return closeErr
}

func shutdownHost(h *host.Host) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.Close(ctx)
}
