package host

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ssh "github.com/charmbracelet/ssh"
	gossh "golang.org/x/crypto/ssh"

	"bomber-cli/internal/room"
)

func connectionCount(h *Host) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

type loadClient struct {
	client  *gossh.Client
	session *gossh.Session
	input   io.WriteCloser
}

func connectLoadClient(addr string, n int) (*loadClient, error) {
	raw, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	// Bound every stage, including SSH handshake, shell startup and writes.
	if err := raw.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		_ = raw.Close()
		return nil, err
	}
	conn, chans, reqs, err := gossh.NewClientConn(raw, addr, &gossh.ClientConfig{
		User: fmt.Sprintf("load-%d", n), HostKeyCallback: gossh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	c := gossh.NewClient(conn, chans, reqs)
	fail := func(err error) (*loadClient, error) { _ = c.Close(); return nil, err }
	s, err := c.NewSession()
	if err != nil {
		return fail(err)
	}
	s.Stdout, s.Stderr = io.Discard, io.Discard
	in, err := s.StdinPipe()
	if err != nil {
		return fail(err)
	}
	if err := s.RequestPty("dumb", 24, 60, nil); err != nil {
		return fail(err)
	}
	if err := s.Shell(); err != nil {
		return fail(err)
	}
	return &loadClient{c, s, in}, nil
}

func churnWave(t *testing.T, h *Host, addr string, count int) {
	t.Helper()
	clients := make([]*loadClient, count)
	errs := make([]error, count)
	defer func() {
		for _, c := range clients {
			if c != nil {
				_ = c.client.Close()
			}
		}
	}()
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() { clients[i], errs[i] = connectLoadClient(addr, i) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range clients {
		if _, err := io.WriteString(c.input, "\r"); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool {
		total := 0
		for _, r := range h.Hub.List() {
			total += r.Count
		}
		return total == count
	})
	if connectionCount(h) != count {
		t.Fatal("connection accounting mismatch")
	}
	if extra, err := connectLoadClient(addr, count); err == nil {
		_ = extra.client.Close()
		t.Fatal("full server accepted extra connection")
	}
	for i, c := range clients {
		wg.Go(func() {
			_, errs[i] = io.WriteString(c.input, strings.Repeat("wasd", 1024))
			if errs[i] != nil {
				return
			}
			for n := range 100 {
				if err := c.session.WindowChange(24, 60+n%2); err != nil {
					errs[i] = err
					return
				}
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Exercise both channel-only disconnect and abrupt transport loss while
	// input and resize queues are still being consumed.
	for i, c := range clients {
		wg.Go(func() {
			if i%2 == 0 {
				_ = c.session.Close()
			} else {
				_ = c.client.Close()
			}
		})
	}
	wg.Wait()
	eventually(t, func() bool { return connectionCount(h) == 0 && len(h.Hub.List()) == 0 })
}

func TestSSHConnectionChurn(t *testing.T) {
	cfg := config(t)
	cfg.MaxSessions, cfg.MaxRooms = 16, 4
	h, addr := start(t, cfg)
	defer func() {
		if t.Failed() {
			var stacks strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&stacks, 2)
			t.Log(stacks.String())
		}
	}()
	idle := runtime.NumGoroutine()
	churnWave(t, h, addr, cfg.MaxSessions) // Warm runtime and library caches.
	// Allow the shared signal-handler goroutine, but do not establish a
	// baseline that silently includes leaked goroutines from the warm-up.
	eventually(t, func() bool { return runtime.NumGoroutine() <= idle+2 })
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	baseline := runtime.NumGoroutine()
	for range 4 {
		churnWave(t, h, addr, cfg.MaxSessions)
	}
	eventually(t, func() bool { return runtime.NumGoroutine() <= baseline+2 })
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("80 SSH sessions, 16 concurrent: retained heap delta %d bytes; goroutines %d -> %d", growth, baseline, runtime.NumGoroutine())
	// Allow cache/scheduler variation while catching retained per-client SSH
	// buffers. This checks post-GC Go heap, not process RSS or peak memory.
	if growth > 8<<20 {
		t.Fatalf("retained heap grew by %d bytes", growth)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// Block a real SSH transport's writes on demand. This deterministically models
// a client that stops reading without relying on OS socket buffer sizes.
type stalledConn struct {
	net.Conn
	stall                atomic.Bool
	blocked              chan struct{}
	closed               chan struct{}
	blockOnce, closeOnce sync.Once
}

func (c *stalledConn) Write(p []byte) (int, error) {
	if c.stall.Load() {
		c.blockOnce.Do(func() { close(c.blocked) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func (c *stalledConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestSSHShutdownWithSlowClientAndActiveMatch(t *testing.T) {
	h, err := New(config(t))
	if err != nil {
		t.Fatal(err)
	}
	original := h.Server.ConnCallback
	wrapped := make(chan *stalledConn, 1)
	var first atomic.Bool
	h.Server.ConnCallback = func(ctx ssh.Context, c net.Conn) net.Conn {
		if first.CompareAndSwap(false, true) {
			slow := &stalledConn{Conn: c, blocked: make(chan struct{}), closed: make(chan struct{})}
			wrapped <- slow
			c = slow
		}
		return original(ctx, c)
	}
	_, addr := serveHost(t, h)
	slowClient := dial(t, addr, "slow")
	_, slowInput, _ := shell(t, slowClient)
	slow := <-wrapped
	if _, err := io.WriteString(slowInput, "\r"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(h.Hub.List()) == 1 })
	fast := dial(t, addr, "fast")
	_, fastInput, fastOutput := shell(t, fast)
	if _, err := io.WriteString(fastInput, "\r"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { rs := h.Hub.List(); return len(rs) == 1 && rs[0].Count == 2 })
	if _, err := io.WriteString(slowInput, "\r"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fastInput, "\r"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return h.Hub.List()[0].Phase == room.Playing })
	slow.stall.Store(true)
	if _, err := io.WriteString(fastInput, " "); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return h.Hub.List()[0].Game.BombCount == 1 })
	select {
	case <-slow.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("failed to block SSH output")
	}
	if _, err := io.WriteString(fastInput, "a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return h.Hub.List()[0].Game.Players[1].Pos.X == 12 })
	eventually(t, func() bool { return strings.Contains(fastOutput.text(), "PLAYING") })
	// Also leave an uncompleted SSH handshake open during shutdown.
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	eventually(t, func() bool { return connectionCount(h) == 3 })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if connectionCount(h) != 0 || len(h.Hub.List()) != 0 {
		t.Fatal("shutdown retained connections or rooms")
	}
	if _, err := h.Hub.Connect("late"); err == nil {
		t.Fatal("shutdown accepted a player")
	}
}

func TestSSHHandshakeChurnAndShutdown(t *testing.T) {
	cfg := config(t)
	cfg.MaxSessions = 8
	cfg.HandshakeTimeout = 500 * time.Millisecond
	h, addr := start(t, cfg)
	for range 3 {
		clients := make([]net.Conn, 0, cfg.MaxSessions)
		for range cfg.MaxSessions {
			c, err := net.DialTimeout("tcp", addr, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			clients = append(clients, c)
		}
		eventually(t, func() bool { return connectionCount(h) == cfg.MaxSessions })
		for _, c := range clients {
			if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, err := io.Copy(io.Discard, c)
			_ = c.Close()
			if err != nil {
				t.Fatal("handshake did not close before read deadline:", err)
			}
		}
		eventually(t, func() bool { return connectionCount(h) == 0 })
	}
	// A legitimate client can reuse the slots exhausted by stalled handshakes.
	c := dial(t, addr, "recovered")
	shell(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = h.Close(ctx) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal("concurrent close:", err)
		}
	}
	if connectionCount(h) != 0 {
		t.Fatal("shutdown retained connection slots")
	}
}
