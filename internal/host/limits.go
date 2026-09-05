package host

import (
	"errors"
	"io"
	"time"
)

// Each budget belongs to one reader; no refill goroutine or unbounded history.
type byteBudget struct {
	rate, burst, tokens float64
	updated             time.Time
}

func newByteBudget(rate, burst int) byteBudget {
	return byteBudget{rate: float64(rate), burst: float64(burst), tokens: float64(burst), updated: time.Now()}
}

func (b *byteBudget) allow(n int) bool {
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.updated).Seconds()*b.rate)
	b.updated = now
	if float64(n) > b.tokens {
		return false
	}
	b.tokens -= float64(n)
	return true
}

var errInputLimit = errors.New("client input limit exceeded")

// The SSH library sets a connection-wide deadline before every read and write.
// Keep its absolute read deadline, but own write deadlines independently:
// incoming traffic must not extend an already blocked write.
func (c *connection) SetDeadline(t time.Time) error {
	return c.SetReadDeadline(t)
}

func (c *connection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.ingress.allow(n) {
		_ = c.Close()
		return 0, errInputLimit
	}
	return n, err
}

func (c *connection) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if err != nil {
		_ = c.Close()
	}
	return n, err
}

type gameInput struct {
	io.Reader
	connection  *connection
	idle        *time.Timer
	idleTimeout time.Duration
	budget      byteBudget
	pastePrefix int
}

func (r *gameInput) Read(p []byte) (int, error) {
	// Bubble Tea accumulates printable text on full 256-byte reads. Short
	// reads bound those events. Paste buffering ignores short reads entirely,
	// so reject its opening marker, including across transport boundaries.
	n, err := r.Reader.Read(p[:min(len(p), 128)])
	if !r.budget.allow(n) {
		_ = r.connection.Close()
		return 0, errInputLimit
	}
	const paste = "\x1b[200~"
	for _, ch := range p[:n] {
		switch ch {
		case paste[r.pastePrefix]:
			r.pastePrefix++
		case paste[0]:
			r.pastePrefix = 1
		default:
			r.pastePrefix = 0
		}
		if r.pastePrefix == len(paste) {
			r.pastePrefix = 0
			_ = r.connection.Close()
			return 0, errInputLimit
		}
	}
	if n > 0 {
		r.idle.Reset(r.idleTimeout)
	}
	return n, err
}
