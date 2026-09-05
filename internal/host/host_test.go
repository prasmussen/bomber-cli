package host

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"bomber-cli/internal/room"
	gossh "golang.org/x/crypto/ssh"
)

type capture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func ptyPayload(width, height uint32, modes string) []byte {
	return gossh.Marshal(struct {
		Term                                     string
		Width, Height, WidthPixels, HeightPixels uint32
		Modes                                    string
	}{"xterm", width, height, 0, 0, modes})
}

func TestMalformedSessionRequests(t *testing.T) {
	_, addr := start(t, config(t))
	c := dial(t, addr, "malformed")
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, payload := range [][]byte{
		nil,
		ptyPayload(10001, 24, "\x00"),
		ptyPayload(60, ^uint32(0), "\x00"),
		ptyPayload(60, 24, "\x01"),
		append(ptyPayload(60, 24, "\x00"), 0),
	} {
		if ok, err := s.SendRequest("pty-req", true, payload); err != nil || ok {
			t.Fatalf("invalid PTY: accepted=%v error=%v", ok, err)
		}
	}
	if err := s.RequestPty("xterm", 24, 60, nil); err != nil {
		t.Fatal("invalid requests prevented valid PTY:", err)
	}
	if ok, err := s.SendRequest("shell", true, []byte{0}); err != nil || ok {
		t.Fatalf("malformed shell: accepted=%v error=%v", ok, err)
	}
	for _, payload := range [][]byte{nil, make([]byte, 15), gossh.Marshal(struct{ W, H, X, Y uint32 }{^uint32(0), 24, 0, 0})} {
		if ok, err := s.SendRequest("window-change", true, payload); err != nil || ok {
			t.Fatalf("invalid resize: accepted=%v error=%v", ok, err)
		}
	}
	out := &capture{}
	s.Stdout = out
	if err := s.Shell(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(out.text(), "LOBBY") })
}

func TestConnectionCloseStopsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := net.Pipe()
		defer b.Close()
		h := &Host{conns: make(map[*connection]struct{})}
		c := &connection{Conn: a, host: h}
		h.conns[c] = struct{}{}
		fired := false
		c.timer = time.AfterFunc(time.Second, func() { fired = true })
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c.Close()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if fired || len(h.conns) != 0 {
			t.Fatal("closed connection retained timer or connection slot")
		}
	})
}

func FuzzPTYValidation(f *testing.F) {
	f.Add(ptyPayload(60, 24, "\x00"))
	f.Add([]byte{})
	f.Add(ptyPayload(^uint32(0), 24, "\x01"))
	f.Fuzz(func(t *testing.T, payload []byte) { validPTY(payload) })
}

func (c *capture) Write(p []byte) (int, error) { c.mu.Lock(); defer c.mu.Unlock(); return c.b.Write(p) }
func (c *capture) text() string                { c.mu.Lock(); defer c.mu.Unlock(); return c.b.String() }
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
func start(t *testing.T, cfg Config) (*Host, string) {
	t.Helper()
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return serveHost(t, h)
}

func serveHost(t *testing.T, h *Host) (*Host, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.Hub.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.Serve(l) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.Close(ctx); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Error("server failed to shut down")
		}
	})
	return h, l.Addr().String()
}
func config(t *testing.T) Config {
	c := DefaultConfig()
	c.HostKey = filepath.Join(t.TempDir(), "keys", "host")
	return c
}
func dial(t *testing.T, addr, name string) *gossh.Client {
	t.Helper()
	c, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{User: name, HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func shell(t *testing.T, c *gossh.Client) (*gossh.Session, io.WriteCloser, *capture) {
	t.Helper()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	out := &capture{}
	s.Stdout = out
	s.Stderr = out
	in, err := s.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RequestPty("xterm-256color", 24, 60, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err = s.Shell(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(out.text(), "LOBBY") })
	return s, in, out
}
func TestRealSSHMultiplayerResizeAndDisconnect(t *testing.T) {
	h, addr := start(t, config(t))
	a := dial(t, addr, "alice")
	b := dial(t, addr, "alice")
	sa, ia, oa := shell(t, a)
	_, ib, ob := shell(t, b)
	if !strings.Contains(ob.text(), "alice-2") {
		t.Fatal("duplicate name not disambiguated")
	}
	io.WriteString(ia, "\r")
	eventually(t, func() bool { return len(h.Hub.List()) == 1 && h.Hub.List()[0].Count == 1 })
	io.WriteString(ib, "\r")
	eventually(t, func() bool { return h.Hub.List()[0].Count == 2 })
	io.WriteString(ia, "\r")
	io.WriteString(ib, "\r")
	eventually(t, func() bool { return h.Hub.List()[0].Phase == room.Countdown })
	eventually(t, func() bool { return h.Hub.List()[0].Phase == room.Playing })
	eventually(t, func() bool { return strings.Contains(oa.text(), "P1") && strings.Contains(ob.text(), "P2") })
	// A 30-column arena row must erase the remaining columns of the 60-column
	// PTY, including lobby text. Check before any resize can initialize the
	// renderer accidentally.
	clearedArenaRow := strings.Repeat("\x1b[34m##\x1b[0m", 15) + "\x1b[K"
	eventually(t, func() bool { return strings.Contains(oa.text(), clearedArenaRow) })
	// Measure real keypress-to-render latency on the guaranteed clear spawn
	// exit. Alternate back to the spawn so the rest of the match test is stable.
	var totalLatency, worstLatency time.Duration
	for move := 0; move < 8; move++ {
		time.Sleep(110 * time.Millisecond)
		key, prefix := "d", "\x1b[34m##\x1b[0m\x1b[37m  \x1b[0m\x1b[36mP1"
		if move%2 == 1 {
			key, prefix = "a", "\x1b[34m##\x1b[0m\x1b[36mP1"
		}
		offset := len(oa.text())
		pressed := time.Now()
		io.WriteString(ia, key)
		eventually(t, func() bool { return strings.Contains(oa.text()[offset:], prefix) })
		latency := time.Since(pressed)
		totalLatency += latency
		worstLatency = max(worstLatency, latency)
	}
	t.Logf("SSH movement keypress-to-render: mean %s, worst %s (8 moves)", totalLatency/8, worstLatency)
	if err := sa.WindowChange(15, 40); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(oa.text(), "Resize terminal") })
	io.WriteString(ia, " d")
	time.Sleep(150 * time.Millisecond)
	snap := h.Hub.List()[0]
	if snap.Game.BombCount != 0 || snap.Game.Players[0].Pos.X != 1 {
		t.Fatal("small terminal accepted gameplay")
	}
	if err := sa.WindowChange(24, 60); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	io.WriteString(ia, " ")
	eventually(t, func() bool { return h.Hub.List()[0].Game.BombCount == 1 })
	b.Close()
	eventually(t, func() bool { return h.Hub.List()[0].Phase == room.Result })
	snap = h.Hub.List()[0]
	if snap.Members[0].Score != 1 {
		t.Fatal("disconnect did not produce winner")
	}
	io.WriteString(ia, "\x1b")
	eventually(t, func() bool { return len(h.Hub.List()) == 0 })
	io.WriteString(ia, "\x03")
	finished := make(chan error, 1)
	go func() { finished <- sa.Wait() }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C failed to disconnect")
	}
}
func TestDeniedCapabilities(t *testing.T) {
	_, addr := start(t, config(t))
	t.Run("no PTY", func(t *testing.T) {
		c := dial(t, addr, "no-pty")
		s, _ := c.NewSession()
		defer s.Close()
		if s.Shell() == nil {
			t.Fatal("shell without PTY accepted")
		}
	})
	for _, kind := range []string{"exec", "sftp", "scp", "agent", "x11", "env"} {
		t.Run(kind, func(t *testing.T) {
			c := dial(t, addr, kind)
			s, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.RequestPty("xterm", 24, 60, nil); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "exec":
				err = s.Start("id")
			case "scp":
				err = s.Start("scp -t /tmp/file")
			case "sftp":
				err = s.RequestSubsystem("sftp")
			default:
				request := map[string]string{"agent": "auth-agent-req@openssh.com", "x11": "x11-req", "env": "env"}[kind]
				var ok bool
				ok, err = s.SendRequest(request, true, nil)
				if err == nil && !ok {
					return
				}
			}
			if err == nil {
				t.Fatal("capability accepted")
			}
		})
	}
	c := dial(t, addr, "forward")
	if ch, err := c.Dial("tcp", "127.0.0.1:22"); err == nil {
		ch.Close()
		t.Fatal("local forwarding accepted")
	}
	if l, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		l.Close()
		t.Fatal("reverse forwarding accepted")
	}
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s2, err := c.NewSession(); err == nil {
		s2.Close()
		t.Fatal("second channel accepted")
	}
}
func TestHostKeyPersistencePermissionsAndLimits(t *testing.T) {
	cfg := config(t)
	cfg.MaxSessions = 1
	cfg.HandshakeTimeout = 150 * time.Millisecond
	h, addr := start(t, cfg)
	before, err := os.ReadFile(cfg.HostKey)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(cfg.HostKey)
	if info.Mode().Perm() != 0600 {
		t.Fatal("key permissions")
	}
	again, err := hostKey(cfg.HostKey)
	if err != nil || !bytes.Equal(before, again) {
		t.Fatal("host key changed")
	}
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	eventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.conns) == 1 })
	blocked, err := gossh.Dial("tcp", addr, &gossh.ClientConfig{User: "extra", HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if err == nil {
		blocked.Close()
		t.Fatal("connection limit ignored")
	}
	eventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.conns) == 0 })
	c := dial(t, addr, "after-timeout")
	shell(t, c)
}
func TestInvalidConfigAndHostKey(t *testing.T) {
	cfg := config(t)
	cfg.MaxRooms = 0
	if _, err := New(cfg); err == nil {
		t.Fatal("invalid limit accepted")
	}
	path := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(path, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg = config(t)
	cfg.HostKey = path
	if _, err := New(cfg); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestResizeBeforeShellAndFlood(t *testing.T) {
	h, addr := start(t, config(t))
	c := dial(t, addr, "resize")
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := &capture{}
	s.Stdout = out
	s.Stderr = out
	if err = s.RequestPty("xterm", 24, 60, nil); err != nil {
		t.Fatal(err)
	}
	// A resize before shell must not block shell startup.
	for n := 0; n < 100; n++ {
		if err = s.WindowChange(15, 40); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Shell(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return strings.Contains(out.text(), "Resize terminal") })
	for n := 0; n < 100; n++ {
		if err = s.WindowChange(24, 60); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return strings.Contains(out.text(), "LOBBY") })
	c.Close()
	eventually(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.conns) == 0 })
}

func TestChannelCloseDisconnectsPlayer(t *testing.T) {
	h, addr := start(t, config(t))
	c := dial(t, addr, "channel-close")
	s, in, _ := shell(t, c)
	io.WriteString(in, "\r")
	eventually(t, func() bool { return len(h.Hub.List()) == 1 })
	// Closing only the SSH channel must release the player even if its transport
	// remains open. Some multiplexing SSH clients keep the transport alive.
	s.Close()
	eventually(t, func() bool { return len(h.Hub.List()) == 0 })
}
