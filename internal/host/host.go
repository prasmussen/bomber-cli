// Package host restricts SSH to interactive game sessions.
package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"bomber-cli/internal/room"
	"bomber-cli/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
	ssh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	wishtea "github.com/charmbracelet/wish/bubbletea"
	gossh "golang.org/x/crypto/ssh"
)

type Config struct {
	Listen, HostKey       string
	MaxSessions, MaxRooms int
	HandshakeTimeout      time.Duration
}

func DefaultConfig() Config {
	return Config{Listen: ":2323", HostKey: "./data/ssh_host_ed25519_key", MaxSessions: 128, MaxRooms: 32, HandshakeTimeout: 10 * time.Second}
}

type Host struct {
	Server   *ssh.Server
	Hub      *room.Hub
	mu       sync.Mutex
	conns    map[*connection]struct{}
	stopping bool
	limit    int
}
type connKey struct{}
type connection struct {
	net.Conn
	host    *Host
	once    sync.Once
	timer   *time.Timer
	channel atomic.Bool
	hasPTY  atomic.Bool
	windows chan ssh.Window
}

func (c *connection) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.host.mu.Lock(); delete(c.host.conns, c); c.host.mu.Unlock() })
	return err
}
func hostKey(path string) ([]byte, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("host key must be a regular file")
		}
		if err = os.Chmod(path, 0600); err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := gossh.MarshalPrivateKey(private, "")
	if err != nil {
		return nil, err
	}
	data := pem.EncodeToMemory(block)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}
func New(cfg Config) (*Host, error) {
	if cfg.MaxSessions < 1 || cfg.MaxRooms < 1 {
		return nil, errors.New("session and room limits must be positive")
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	key, err := hostKey(cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	h := &Host{Hub: room.New(cfg.MaxSessions, cfg.MaxRooms), conns: make(map[*connection]struct{}), limit: cfg.MaxSessions}
	server, err := wish.NewServer(wish.WithAddress(cfg.Listen), wish.WithHostKeyPEM(key))
	if err != nil {
		h.Hub.Close()
		return nil, err
	}
	h.Server = server
	server.ConnCallback = func(ctx ssh.Context, c net.Conn) net.Conn {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.stopping || len(h.conns) >= h.limit {
			return nil
		}
		wrapped := &connection{Conn: c, host: h, windows: make(chan ssh.Window, 1)}
		h.conns[wrapped] = struct{}{}
		wrapped.timer = time.AfterFunc(cfg.HandshakeTimeout, func() { _ = wrapped.Close() })
		ctx.SetValue(connKey{}, wrapped)
		return wrapped
	}
	server.PtyCallback = func(ctx ssh.Context, pty ssh.Pty) bool {
		ctx.Value(connKey{}).(*connection).hasPTY.Store(true)
		return true
	}
	server.SessionRequestCallback = func(s ssh.Session, kind string) bool {
		_, _, pty := s.Pty()
		if kind != "shell" || !pty || s.RawCommand() != "" {
			return false
		}
		s.Context().Value(connKey{}).(*connection).timer.Stop()
		return true
	}
	server.ChannelHandlers = map[string]ssh.ChannelHandler{"session": func(srv *ssh.Server, conn *gossh.ServerConn, ch gossh.NewChannel, ctx ssh.Context) {
		c := ctx.Value(connKey{}).(*connection)
		if !c.channel.CompareAndSwap(false, true) {
			_ = ch.Reject(gossh.ResourceShortage, "one session per connection")
			return
		}
		ssh.DefaultSessionHandler(srv, conn, filteredChannel{NewChannel: ch, ctx: ctx}, ctx)
	}}
	// Empty maps explicitly disable forwarding and subsystems, including SFTP.
	server.RequestHandlers = map[string]ssh.RequestHandler{}
	server.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	server.Handler = func(s ssh.Session) {
		player, err := h.Hub.Connect(s.User())
		if err != nil {
			_, _ = fmt.Fprintln(s, err)
			_ = s.Exit(1)
			return
		}
		defer h.Hub.Disconnect(player)
		pty, _, _ := s.Pty()
		windows := s.Context().Value(connKey{}).(*connection).windows
		ctx, cancel := context.WithCancel(s.Context())
		defer cancel()
		opts := append(wishtea.MakeOptions(s), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithFPS(20))
		program := tea.NewProgram(ui.New(h.Hub, player, pty.Window.Width, pty.Window.Height, pty.Term != "dumb"), opts...)
		go func() {
			// SSH output is not a local terminal, so Bubble Tea cannot discover
			// its dimensions. Initialize the renderer as well as the model.
			program.Send(tea.WindowSizeMsg{Width: pty.Window.Width, Height: pty.Window.Height})
			for {
				select {
				case <-ctx.Done():
					return
				case w, ok := <-windows:
					if !ok {
						return
					}
					program.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
				}
			}
		}()
		if _, err = program.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
			slog.Debug("terminal ended", "session", player.ID, "error", err)
		}
	}
	return h, nil
}

// Wish's underlying SSH session accepts agent forwarding by default. Filter all
// session requests before it sees them; only PTY sizing and shell are supported.
type filteredChannel struct {
	gossh.NewChannel
	ctx context.Context
}

func (f filteredChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	ch, requests, err := f.NewChannel.Accept()
	if err != nil {
		return nil, nil, err
	}
	out := make(chan *gossh.Request)
	go func() {
		// This endpoint allows one channel per connection. Closing that channel
		// must also cancel the connection context used by Bubble Tea.
		defer f.ctx.Value(connKey{}).(*connection).Close()
		defer close(out)
		for {
			select {
			case <-f.ctx.Done():
				return
			case r, ok := <-requests:
				if !ok {
					return
				}
				switch r.Type {
				case "window-change":
					// The underlying library uses a blocking resize channel and mutates its
					// PTY without synchronization. Own resizes here, including before shell.
					c := f.ctx.Value(connKey{}).(*connection)
					if !c.hasPTY.Load() || len(r.Payload) != 16 {
						_ = r.Reply(false, nil)
						continue
					}
					w := ssh.Window{Width: int(binary.BigEndian.Uint32(r.Payload[:4])), Height: int(binary.BigEndian.Uint32(r.Payload[4:8]))}
					if w.Width > 10000 || w.Height > 10000 {
						_ = r.Reply(false, nil)
						continue
					}
					select {
					case c.windows <- w:
					default:
						select {
						case <-c.windows:
						default:
						}
						select {
						case c.windows <- w:
						default:
						}
					}
					_ = r.Reply(true, nil)
				case "pty-req", "shell", "exec", "subsystem":
					select {
					case out <- r:
					case <-f.ctx.Done():
						return
					}
				default:
					_ = r.Reply(false, nil)
				}
			}
		}
	}()
	return ch, out, nil
}
func (h *Host) Serve(l net.Listener) error { return h.Server.Serve(l) }
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	h.stopping = true
	connections := make([]*connection, 0, len(h.conns))
	for c := range h.conns {
		connections = append(connections, c)
	}
	h.mu.Unlock()
	// Closing transports unblocks terminal readers and slow terminal writers.
	for _, c := range connections {
		c.timer.Stop()
		_ = c.Close()
	}
	err := h.Server.Shutdown(ctx)
	h.Hub.Close()
	return err
}
