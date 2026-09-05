package host

import (
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	ssh "github.com/charmbracelet/ssh"
)

func TestByteBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newByteBudget(100, 200)
		if !b.allow(200) || b.allow(1) {
			t.Fatal("burst limit not enforced")
		}
		time.Sleep(500 * time.Millisecond)
		if !b.allow(50) || b.allow(1) {
			t.Fatal("refill rate not enforced")
		}
		time.Sleep(time.Hour)
		if b.allow(201) || !b.allow(200) {
			t.Fatal("idle refill exceeded burst")
		}
	})
}

func TestSSHPerIPLimit(t *testing.T) {
	cfg := config(t)
	cfg.MaxSessionsPerIP = 1
	h, addr := start(t, cfg)
	c := dial(t, addr, "first")
	shell(t, c)
	if extra, err := connectLoadClient(addr, 2); err == nil {
		_ = extra.client.Close()
		t.Fatal("per-IP limit ignored")
	}
	_ = c.Close()
	eventually(t, func() bool { return connectionCount(h) == 0 })
	shell(t, dial(t, addr, "replacement"))
}

func TestSSHIdleIgnoresKeepalives(t *testing.T) {
	cfg := config(t)
	cfg.IdleTimeout = 300 * time.Millisecond
	h, addr := start(t, cfg)
	c := dial(t, addr, "idle")
	shell(t, c)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	eventually(t, func() bool { return connectionCount(h) == 0 })
	<-done
}

func TestSSHInputAbuseDisconnects(t *testing.T) {
	for _, kind := range []string{"paste", "split paste", "text flood", "transport flood"} {
		t.Run(kind, func(t *testing.T) {
			h, addr := start(t, config(t))
			c := dial(t, addr, "abuse")
			_, in, _ := shell(t, c)
			switch kind {
			case "paste":
				_, _ = io.WriteString(in, "\x1b[200~"+strings.Repeat("w", 1024))
			case "split paste":
				// Split the marker across separate SSH packets/reader calls.
				for _, ch := range []byte("\x1b[200~") {
					_, _ = in.Write([]byte{ch})
					time.Sleep(10 * time.Millisecond)
				}
			case "text flood":
				_, _ = io.WriteString(in, strings.Repeat("w", 32768))
			case "transport flood":
				for range 8 {
					if _, _, err := c.SendRequest("unsupported", true, make([]byte, 60000)); err != nil {
						break
					}
				}
			}
			eventually(t, func() bool { return connectionCount(h) == 0 })
			// Another connection must still work after rejecting the attacker.
			shell(t, dial(t, addr, "healthy"))
		})
	}
}

func TestSlowTransportWriteTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := net.Pipe()
		defer func() { _ = b.Close() }()
		h := &Host{conns: make(map[*connection]struct{})}
		c := &connection{Conn: a, host: h, timer: time.NewTimer(time.Hour)}
		h.conns[c] = struct{}{}
		defer func() { _ = c.Close() }()
		start := time.Now()
		result := make(chan error, 1)
		go func() {
			_, err := c.Write([]byte("blocked"))
			result <- err
		}()
		synctest.Wait()
		// A concurrent SSH read refreshes the library's absolute deadline.
		// That must not postpone the stalled writer's shorter deadline.
		if err := c.SetDeadline(time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := <-result; err == nil {
			t.Fatal("write to non-reading peer succeeded")
		}
		if time.Since(start) != 15*time.Second || connectionCount(h) != 0 {
			t.Fatal("slow write did not time out and release slot")
		}
	})
}

func TestGameInputBoundsPrintableChunks(t *testing.T) {
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	r := &gameInput{Reader: strings.NewReader(strings.Repeat("a", 8192)),
		idle: idle, idleTimeout: time.Hour, budget: newByteBudget(1024, 8192)}
	buf := make([]byte, 256)
	for range 64 {
		n, err := r.Read(buf)
		if err != nil || n != 128 {
			t.Fatalf("terminal parser received an unbounded chunk: n=%d err=%v", n, err)
		}
	}
}

func TestSSHAdmissionBudget(t *testing.T) {
	h, err := New(config(t))
	if err != nil {
		t.Fatal(err)
	}
	// Keep this budget empty for the duration of the test, then refill it
	// explicitly to verify admission recovery without timing assumptions.
	h.admission = newByteBudget(0, 1)
	h.admission.tokens = 0
	original := h.Server.ConnCallback
	attempted := make(chan struct{}, 1)
	h.Server.ConnCallback = func(ctx ssh.Context, c net.Conn) net.Conn {
		result := original(ctx, c)
		attempted <- struct{}{}
		return result
	}
	_, addr := serveHost(t, h)
	if c, err := connectLoadClient(addr, 1); err == nil {
		_ = c.client.Close()
		t.Fatal("exhausted admission budget accepted connection")
	}
	<-attempted
	if connectionCount(h) != 0 {
		t.Fatal("rejected connection retained a slot")
	}
	h.mu.Lock()
	h.admission.tokens = 1
	h.mu.Unlock()
	c := dial(t, addr, "recovered")
	<-attempted
	shell(t, c)
}
