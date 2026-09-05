// Package host restricts SSH to interactive game sessions.
package host

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	ssh "github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	wishtea "github.com/charmbracelet/wish/bubbletea"
	gossh "golang.org/x/crypto/ssh"

	"bomber-cli/internal/room"
	"bomber-cli/internal/ui"
)

type Config struct {
	Listen, HostKey       string
	MaxSessions, MaxRooms int
	HandshakeTimeout      time.Duration
	IdleTimeout           time.Duration
	MaxSessionsPerIP      int
}

func DefaultConfig() Config {
	return Config{Listen: ":2323", HostKey: "./data/ssh_host_ed25519_key", MaxSessions: 128, MaxRooms: 32, HandshakeTimeout: 10 * time.Second, IdleTimeout: 10 * time.Minute, MaxSessionsPerIP: 16}
}

type Host struct {
	Server    *ssh.Server
	Hub       *room.Hub
	mu        sync.Mutex
	conns     map[*connection]struct{}
	stopping  bool
	limit     int
	perIP     int
	admission byteBudget
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
	ip      string
	ingress byteBudget
}

func (c *connection) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.host.mu.Lock()
		defer c.host.mu.Unlock()
		c.timer.Stop()
		delete(c.host.conns, c)
	})
	return err
}

func New(cfg Config) (*Host, error) {
	if cfg.MaxSessions < 1 || cfg.MaxRooms < 1 {
		return nil, errors.New("session and room limits must be positive")
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout < 0 || cfg.MaxSessionsPerIP < 0 {
		return nil, errors.New("idle timeout and per-IP limit must not be negative")
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 10 * time.Minute
	}
	if cfg.MaxSessionsPerIP == 0 {
		cfg.MaxSessionsPerIP = 16
	}
	key, err := hostKey(cfg.HostKey)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}
	h := &Host{Hub: room.New(cfg.MaxSessions, cfg.MaxRooms), conns: make(map[*connection]struct{}), limit: cfg.MaxSessions, perIP: cfg.MaxSessionsPerIP, admission: newByteBudget(20, 128)}
	server, err := wish.NewServer(wish.WithAddress(cfg.Listen), wish.WithHostKeyPEM(key))
	if err != nil {
		h.Hub.Close()
		return nil, err
	}
	h.Server = server
	h.configureSSH(cfg.HandshakeTimeout, cfg.IdleTimeout)
	return h, nil
}

func (h *Host) configureSSH(handshakeTimeout, idleTimeout time.Duration) {
	h.Server.ConnCallback = func(ctx ssh.Context, conn net.Conn) net.Conn {
		return h.acceptConnection(ctx, conn, handshakeTimeout)
	}
	h.Server.PtyCallback = acceptPTY
	h.Server.SessionRequestCallback = acceptShell
	h.Server.ChannelHandlers = map[string]ssh.ChannelHandler{"session": handleSessionChannel}
	// Empty maps explicitly disable forwarding and subsystems, including SFTP.
	h.Server.RequestHandlers = map[string]ssh.RequestHandler{}
	h.Server.SubsystemHandlers = map[string]ssh.SubsystemHandler{}
	h.Server.MaxTimeout = 4 * time.Hour
	h.Server.Handler = func(s ssh.Session) { h.runGameSession(s, idleTimeout) }
}

func (h *Host) acceptConnection(ctx ssh.Context, c net.Conn, handshakeTimeout time.Duration) net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopping || len(h.conns) >= h.limit {
		return nil
	}
	ip, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		return nil
	}
	if parsed := net.ParseIP(ip); parsed != nil {
		ip = parsed.String()
	}
	count := 0
	for existing := range h.conns {
		if existing.ip == ip {
			count++
		}
	}
	if count >= h.perIP || !h.admission.allow(1) {
		return nil
	}
	wrapped := &connection{Conn: c, host: h, windows: make(chan ssh.Window, 1), ip: ip,
		ingress: newByteBudget(64*1024, 256*1024)}
	h.conns[wrapped] = struct{}{}
	wrapped.timer = time.AfterFunc(handshakeTimeout, func() { _ = wrapped.Close() })
	ctx.SetValue(connKey{}, wrapped)
	return wrapped
}

func acceptPTY(ctx ssh.Context, pty ssh.Pty) bool {
	if !validWindow(pty.Window.Width, pty.Window.Height) {
		return false
	}
	ctx.Value(connKey{}).(*connection).hasPTY.Store(true)
	return true
}

func acceptShell(s ssh.Session, kind string) bool {
	_, _, pty := s.Pty()
	if kind != "shell" || !pty || s.RawCommand() != "" {
		return false
	}
	s.Context().Value(connKey{}).(*connection).timer.Stop()
	return true
}

func handleSessionChannel(srv *ssh.Server, conn *gossh.ServerConn, ch gossh.NewChannel, ctx ssh.Context) {
	c := ctx.Value(connKey{}).(*connection)
	if !c.channel.CompareAndSwap(false, true) {
		_ = ch.Reject(gossh.ResourceShortage, "one session per connection")
		return
	}
	ssh.DefaultSessionHandler(srv, conn, filteredChannel{NewChannel: ch, ctx: ctx}, ctx)
}

func (h *Host) runGameSession(s ssh.Session, idleTimeout time.Duration) {
	player, err := h.Hub.Connect(s.User())
	if err != nil {
		_, _ = fmt.Fprintln(s, err)
		_ = s.Exit(1)
		return
	}
	defer h.Hub.Disconnect(player)
	pty, _, _ := s.Pty()
	c := s.Context().Value(connKey{}).(*connection)
	windows := c.windows
	idle := time.AfterFunc(idleTimeout, func() { _ = c.Close() })
	defer idle.Stop()
	ctx, cancel := context.WithCancel(s.Context())
	defer cancel()
	opts := append(wishtea.MakeOptions(s), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithFPS(60),
		tea.WithoutBracketedPaste(), tea.WithInput(&gameInput{Reader: s, connection: c, idle: idle, idleTimeout: idleTimeout, budget: newByteBudget(1024, 8192)}))
	program := tea.NewProgram(ui.New(h.Hub, player, pty.Window.Width, pty.Window.Height, pty.Term != "dumb"), opts...)
	go forwardSessionUpdates(ctx, program, player.Frames, windows, pty.Window)
	if _, err = program.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		slog.Debug("terminal ended", "session", player.ID, "error", err)
	}
}

func forwardSessionUpdates(ctx context.Context, program *tea.Program, frames <-chan room.Snapshot, windows <-chan ssh.Window, initialWindow ssh.Window) {
	// SSH output is not a local terminal, so Bubble Tea cannot discover
	// its dimensions. Initialize the renderer as well as the model.
	program.Send(tea.WindowSizeMsg{Width: initialWindow.Width, Height: initialWindow.Height})
	for {
		select {
		case <-ctx.Done():
			return
		case snapshot := <-frames:
			program.Send(snapshot)
		case w, ok := <-windows:
			if !ok {
				return
			}
			program.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
		}
	}
}

// Wish's underlying SSH session accepts agent forwarding by default. Filter all
// session requests before it sees them; only PTY sizing and shell are supported.
type filteredChannel struct {
	gossh.NewChannel
	ctx context.Context
}

// Bound client-controlled dimensions before they reach the terminal renderer.
func validWindow(width, height int) bool {
	return width >= 0 && height >= 0 && width <= 10000 && height <= 10000
}

func validPTY(payload []byte) bool {
	var p struct {
		Term                                     string
		Width, Height, WidthPixels, HeightPixels uint32
		Modes                                    string
	}
	if gossh.Unmarshal(payload, &p) != nil || len(p.Term) > 256 || p.Width > 10000 || p.Height > 10000 {
		return false
	}
	return validTerminalModes(p.Modes)
}

func validTerminalModes(modes string) bool {
	// The dependency's parser ignores the encoded modes' declared length.
	// Validate the envelope and mode arguments before handing it the request.
	for remaining := []byte(modes); len(remaining) > 0; {
		opcode := remaining[0]
		remaining = remaining[1:]
		if opcode == 0 || opcode >= 160 {
			return true
		}
		if len(remaining) < 4 {
			return false
		}
		remaining = remaining[4:]
	}
	return len(modes) == 0
}

func (f filteredChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	ch, requests, err := f.NewChannel.Accept()
	if err != nil {
		return nil, nil, err
	}
	out := make(chan *gossh.Request)
	go f.filterRequests(requests, out)
	return ch, out, nil
}

func (f filteredChannel) filterRequests(requests <-chan *gossh.Request, out chan<- *gossh.Request) {
	defer f.closeConnectionAndDrainRequests(requests)
	defer close(out)
	for {
		select {
		case <-f.ctx.Done():
			return
		case request, ok := <-requests:
			if !ok {
				return
			}
			if !f.handleRequest(request, out) {
				return
			}
		}
	}
}

func (f filteredChannel) closeConnectionAndDrainRequests(requests <-chan *gossh.Request) {
	// Closing the only channel must also cancel Bubble Tea's connection context.
	_ = f.ctx.Value(connKey{}).(*connection).Close()
	// Drain buffered requests so the SSH mux can observe EOF after socket closure.
	for range requests {
	}
}

func (f filteredChannel) handleRequest(request *gossh.Request, out chan<- *gossh.Request) bool {
	switch request.Type {
	case "window-change":
		c := f.ctx.Value(connKey{}).(*connection)
		_ = request.Reply(c.resizeTerminal(request.Payload), nil)
	case "pty-req", "shell":
		if request.Type == "pty-req" && !validPTY(request.Payload) || request.Type == "shell" && len(request.Payload) != 0 {
			_ = request.Reply(false, nil)
			return true
		}
		select {
		case out <- request:
		case <-f.ctx.Done():
			return false
		}
	default:
		_ = request.Reply(false, nil)
	}
	return true
}

func (c *connection) resizeTerminal(payload []byte) bool {
	// Own resizes, including before shell: the library blocks on delivery and
	// mutates its PTY without synchronization.
	if !c.hasPTY.Load() || len(payload) != 16 {
		return false
	}
	window := ssh.Window{
		Width:  int(binary.BigEndian.Uint32(payload[:4])),
		Height: int(binary.BigEndian.Uint32(payload[4:8])),
	}
	if !validWindow(window.Width, window.Height) {
		return false
	}
	c.publishLatestWindow(window)
	return true
}

func (c *connection) publishLatestWindow(window ssh.Window) {
	select {
	case c.windows <- window:
	default:
		select {
		case <-c.windows:
		default:
		}
		select {
		case c.windows <- window:
		default:
		}
	}
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
