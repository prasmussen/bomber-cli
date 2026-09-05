// Command loadtest drives real SSH sessions against an isolated test server.
// Run with scripts/loadtest.py; never point it at a public/shared server.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ssh "golang.org/x/crypto/ssh"
)

type observer struct {
	mu           sync.Mutex
	tail, needle string
	ready        chan time.Time
	bytes        atomic.Int64
}

func (o *observer) Write(p []byte) (int, error) {
	o.bytes.Add(int64(len(p)))
	o.mu.Lock()
	defer o.mu.Unlock()
	// Output matching is bounded and survives SSH packet boundaries.
	o.tail += string(p)
	if o.needle != "" && strings.Contains(o.tail, o.needle) {
		o.ready <- time.Now()
		o.needle = ""
	}
	if len(o.tail) > 16384 {
		o.tail = o.tail[len(o.tail)-8192:]
	}
	return len(p), nil
}

func (o *observer) arm(needle string) <-chan time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tail, o.needle = "", needle
	o.ready = make(chan time.Time, 1)
	return o.ready
}

type client struct {
	conn    *ssh.Client
	raw     net.Conn
	session *ssh.Session
	in      io.WriteCloser
	out     *observer
	done    chan struct{}
}

func connect(address string, id int) (*client, error) {
	// Linux loopback provides distinct source IPs without weakening server
	// policy. The generator shares only the server's network namespace.
	dialer := net.Dialer{Timeout: 5 * time.Second,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(fmt.Sprintf("127.0.0.%d", 2+id/16))}}
	raw, err := dialer.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = raw.Close()
		}
	}()
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, err
	}
	conn, chans, reqs, err := ssh.NewClientConn(raw, address, &ssh.ClientConfig{
		User: fmt.Sprintf("load-%03d", id), HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		return nil, err
	}
	c := &client{conn: ssh.NewClient(conn, chans, reqs), raw: raw, out: &observer{}, done: make(chan struct{})}
	c.session, err = c.conn.NewSession()
	if err != nil {
		return nil, err
	}
	c.session.Stdout, c.session.Stderr = c.out, c.out
	c.in, err = c.session.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err = c.session.RequestPty("xterm-256color", 24, 60, nil); err != nil {
		return nil, err
	}
	ready := c.out.arm("LOBBY")
	if err = c.session.Shell(); err != nil {
		return nil, err
	}
	go func() { _ = c.session.Wait(); close(c.done) }()
	if _, err = c.wait(ready, time.Now()); err != nil {
		return nil, err
	}
	if err = raw.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ok = true
	return c, nil
}

func (c *client) wait(ready <-chan time.Time, start time.Time) (float64, error) {
	select {
	case at := <-ready:
		return float64(at.Sub(start)) / float64(time.Millisecond), nil
	case <-c.done:
		return 0, errors.New("SSH session disconnected")
	case <-time.After(5 * time.Second):
		return 0, errors.New("expected terminal output timed out")
	}
}

func (c *client) key(key string) error {
	if err := c.raw.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	_, err := io.WriteString(c.in, key)
	return err
}

func (c *client) command(key, expected string) (float64, error) {
	ready := c.out.arm(expected)
	start := time.Now()
	if err := c.key(key); err != nil {
		return 0, err
	}
	return c.wait(ready, start)
}

func event(kind string, fields map[string]any) {
	fields["event"], fields["time"] = kind, time.Now().UTC().Format(time.RFC3339Nano)
	_ = json.NewEncoder(os.Stdout).Encode(fields)
}

func distribution(samples []float64) map[string]any {
	if len(samples) == 0 {
		return map[string]any{"count": 0}
	}
	slices.Sort(samples)
	total := 0.0
	for _, n := range samples {
		total += n
	}
	return map[string]any{"count": len(samples), "mean_ms": total / float64(len(samples)),
		"p50_ms": samples[(len(samples)-1)*50/100], "p95_ms": samples[(len(samples)-1)*95/100],
		"p99_ms": samples[(len(samples)-1)*99/100], "max_ms": samples[len(samples)-1]}
}

func population(address string, roomBase int) ([]*client, []float64, error) {
	clients := make([]*client, 0, 128)
	var latencies []float64
	for id := range 128 {
		start := time.Now()
		c, err := connect(address, id)
		if err != nil {
			return clients, latencies, fmt.Errorf("connect %d: %w", id, err)
		}
		clients = append(clients, c)
		latencies = append(latencies, float64(time.Since(start))/float64(time.Millisecond))
		if _, err := c.command("\r", fmt.Sprintf("ROOM %d  WAITING", roomBase+id/4)); err != nil {
			return clients, latencies, fmt.Errorf("join %d: %w", id, err)
		}
		if len(clients) == 16 || len(clients) == 64 || len(clients) == 128 {
			event("ramp", map[string]any{"sessions": len(clients), "rooms": len(clients) / 4})
		}
		// Stay below the steady admission budget during reconnect waves.
		time.Sleep(55 * time.Millisecond)
	}
	return clients, latencies, nil
}

func startGames(clients []*client) error {
	ready := make([]<-chan time.Time, len(clients))
	for i, c := range clients {
		ready[i] = c.out.arm("PLAYING")
		if err := c.key("\r"); err != nil {
			return err
		}
	}
	for i, c := range clients {
		if _, err := c.wait(ready[i], time.Now()); err != nil {
			return fmt.Errorf("start %d: %w", i, err)
		}
	}
	return nil
}

func closeClients(clients []*client) {
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Go(func() { _ = c.conn.Close() })
	}
	wg.Wait()
}

func exercise(clients []*client, duration time.Duration) ([]float64, int64, error) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var samples []float64
	var inputs atomic.Int64
	errs := make(chan error, len(clients))
	deadline := time.Now().Add(duration)
	for id, c := range clients {
		wg.Go(func() {
			ticker := time.NewTicker(150 * time.Millisecond)
			defer ticker.Stop()
			step := 0
			for time.Now().Before(deadline) {
				<-ticker.C
				key := "d"
				if (id%4 == 1 || id%4 == 2) != (step%2 == 1) {
					key = "a"
				}
				var err error
				if id%4 == 0 {
					// P1 walks between the spawn and its guaranteed clear exit.
					// Match the changed arena row, not an unrelated frame or echo.
					prefix := "\x1b[34m##\x1b[0m\x1b[36mP1"
					if step%2 == 0 {
						prefix = "\x1b[34m##\x1b[0m\x1b[37m  \x1b[0m\x1b[36mP1"
					}
					var latency float64
					latency, err = c.command(key, prefix)
					if err == nil {
						mu.Lock()
						samples = append(samples, latency)
						mu.Unlock()
					}
				} else {
					err = c.key(key)
				}
				if err != nil {
					errs <- fmt.Errorf("player %d: %w", id, err)
					return
				}
				inputs.Add(1)
				step++
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return samples, inputs.Load(), err
	}
	for id, c := range clients {
		select {
		case <-c.done:
			return samples, inputs.Load(), fmt.Errorf("player %d disconnected", id)
		default:
		}
	}
	return samples, inputs.Load(), nil
}

func bombsAndRematch(clients []*client) error {
	ready := make([]<-chan time.Time, len(clients))
	for i, c := range clients {
		ready[i] = c.out.arm("RESULT")
		if err := c.key(" "); err != nil {
			return err
		}
	}
	for i, c := range clients {
		if _, err := c.wait(ready[i], time.Now()); err != nil {
			return err
		}
	}
	// Results last three seconds; allow the waiting state to render everywhere.
	time.Sleep(4 * time.Second)
	return startGames(clients)
}

func run(address string, duration time.Duration) error {
	event("begin", map[string]any{"sessions": 128, "source_ips": 8, "movement_duration_s": duration.Seconds()})
	clients, startup, err := population(address, 1)
	defer func() { closeClients(clients) }()
	if err != nil {
		return err
	}
	event("connected", map[string]any{"startup": distribution(startup)})
	rejected := 0
	for id := 128; id < 144; id++ {
		extra, err := connect(address, id)
		if err == nil {
			_ = extra.conn.Close()
			return errors.New("server accepted more than 128 sessions")
		}
		rejected++
	}
	event("capacity", map[string]any{"excess_rejected": rejected})
	if err := startGames(clients); err != nil {
		return err
	}
	event("movement_start", map[string]any{"sessions": 128, "active_rooms": 32})
	samples, inputs, err := exercise(clients, duration)
	if err != nil {
		return err
	}
	output := int64(0)
	for _, c := range clients {
		output += c.out.bytes.Load()
	}
	event("movement_complete", map[string]any{"inputs": inputs, "output_bytes": output, "movement_latency": distribution(samples)})
	if err := bombsAndRematch(clients); err != nil {
		return fmt.Errorf("bomb/rematch: %w", err)
	}
	event("rematch", map[string]any{"active_rooms": 32, "sessions": 128})
	closeClients(clients)
	event("disconnected", map[string]any{})
	time.Sleep(10 * time.Second)
	clients, startup, err = population(address, 33)
	if err != nil {
		return fmt.Errorf("reconnect: %w", err)
	}
	if err := startGames(clients); err != nil {
		return err
	}
	event("reconnected", map[string]any{"sessions": len(clients), "startup": distribution(startup)})
	samples, inputs, err = exercise(clients, 10*time.Second)
	if err != nil {
		return err
	}
	event("recovery", map[string]any{"inputs": inputs, "movement_latency": distribution(samples)})
	closeClients(clients)
	event("final_disconnect", map[string]any{})
	time.Sleep(10 * time.Second)
	event("pass", map[string]any{"total_sessions": 256})
	return nil
}

func main() {
	address := flag.String("address", "127.0.0.1:2323", "isolated server address")
	duration := flag.Duration("duration", 2*time.Minute, "movement phase duration (maximum 2m30s)")
	flag.Parse()
	if *duration <= 0 || *duration > 150*time.Second {
		fmt.Fprintln(os.Stderr, "duration must be in (0, 2m30s]")
		os.Exit(2)
	}
	if err := run(*address, *duration); err != nil {
		event("fail", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}
