// Package game implements deterministic Bomberman rules. All time comes from callers.
package game

import (
	"math/rand"
	"time"
)

const (
	Width      = 15
	Height     = 13
	MaxPlayers = 4
)

type Tile uint8

const (
	Floor Tile = iota
	Wall
	Block
)

type Power uint8

const (
	None Power = iota
	Capacity
	Range
	Speed
)

type Pos struct{ X, Y int }

var Spawns = [4]Pos{{1, 1}, {13, 11}, {13, 1}, {1, 11}}

type Player struct {
	ID              uint64
	Pos             Pos
	Alive           bool
	Capacity, Range int
	Cooldown        time.Duration
	NextMove        time.Time
}
type Bomb struct {
	Pos   Pos
	Owner uint64
	Range int
	Due   time.Time
}

// View is a value-only snapshot: copying it never shares mutable game state.
type View struct {
	Tiles        [Height][Width]Tile
	Powers       [Height][Width]Power
	Flames       [Height][Width]time.Time
	Players      [4]Player
	Bombs        [20]Bomb
	BombCount    int
	Started, Now time.Time
	Over         bool
	Winner       uint64 // Zero denotes a draw.
}
type Game struct {
	View
	rng *rand.Rand
}

func New(ids []uint64, now time.Time, seed int64) *Game {
	g := &Game{View: View{Started: now, Now: now}, rng: rand.New(rand.NewSource(seed))}
	for y := 0; y < Height; y++ {
		for x := 0; x < Width; x++ {
			if x == 0 || y == 0 || x == Width-1 || y == Height-1 || x%2 == 0 && y%2 == 0 {
				g.Tiles[y][x] = Wall
			}
		}
	}
	// Reflect every sampled block across both axes, preserving fair corner starts.
	for y := 1; y <= Height/2; y++ {
		for x := 1; x <= Width/2; x++ {
			if g.Tiles[y][x] == Floor && g.rng.Intn(100) < 65 {
				for _, p := range []Pos{{x, y}, {Width - 1 - x, y}, {x, Height - 1 - y}, {Width - 1 - x, Height - 1 - y}} {
					g.Tiles[p.Y][p.X] = Block
				}
			}
		}
	}
	for _, p := range Spawns {
		for _, d := range []Pos{{0, 0}, {1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			q := Pos{p.X + d.X, p.Y + d.Y}
			if g.Tiles[q.Y][q.X] != Wall {
				g.Tiles[q.Y][q.X] = Floor
			}
		}
	}
	for i, id := range ids {
		if i >= 4 {
			break
		}
		g.Players[i] = Player{ID: id, Pos: Spawns[i], Alive: true, Capacity: 1, Range: 2, Cooldown: 100 * time.Millisecond}
	}
	return g
}
func (g *Game) player(id uint64) *Player {
	for i := range g.Players {
		if id != 0 && g.Players[i].ID == id {
			return &g.Players[i]
		}
	}
	return nil
}
func (g *Game) bombAt(p Pos) int {
	for i := 0; i < g.BombCount; i++ {
		if g.Bombs[i].Pos == p {
			return i
		}
	}
	return -1
}
func (g *Game) Move(id uint64, dx, dy int, now time.Time) bool {
	p := g.player(id)
	if g.Over || p == nil || !p.Alive || now.Before(p.NextMove) || abs(dx)+abs(dy) != 1 {
		return false
	}
	q := Pos{p.Pos.X + dx, p.Pos.Y + dy}
	if q.X < 0 || q.Y < 0 || q.X >= Width || q.Y >= Height || g.Tiles[q.Y][q.X] != Floor || g.bombAt(q) >= 0 {
		return false
	}
	p.Pos = q
	p.NextMove = now.Add(p.Cooldown)
	if now.Before(g.Flames[q.Y][q.X]) {
		p.Alive = false
		return true
	}
	switch g.Powers[q.Y][q.X] {
	case Capacity:
		if p.Capacity < 5 {
			p.Capacity++
		}
	case Range:
		if p.Range < 8 {
			p.Range++
		}
	case Speed:
		if p.Cooldown > 25*time.Millisecond {
			p.Cooldown -= 25 * time.Millisecond
		}
	}
	g.Powers[q.Y][q.X] = None
	return true
}
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
func (g *Game) Place(id uint64, now time.Time) bool {
	p := g.player(id)
	if g.Over || p == nil || !p.Alive || g.bombAt(p.Pos) >= 0 || g.BombCount == len(g.Bombs) {
		return false
	}
	n := 0
	for i := 0; i < g.BombCount; i++ {
		if g.Bombs[i].Owner == id {
			n++
		}
	}
	if n >= p.Capacity {
		return false
	}
	g.Bombs[g.BombCount] = Bomb{p.Pos, id, p.Range, now.Add(2 * time.Second)}
	g.BombCount++
	return true
}
func (g *Game) Remove(id uint64) {
	if p := g.player(id); p != nil {
		p.Alive = false
	}
}
func (g *Game) Tick(now time.Time) {
	if g.Over {
		return
	}
	g.Now = now
	for y := range g.Flames {
		for x, t := range g.Flames[y] {
			if !now.Before(t) {
				g.Flames[y][x] = time.Time{}
			}
		}
	}
	// Remove a bomb before propagating its blast so recursive chain reactions terminate.
	for i := 0; i < g.BombCount; {
		if !now.Before(g.Bombs[i].Due) || now.Before(g.Flames[g.Bombs[i].Pos.Y][g.Bombs[i].Pos.X]) {
			g.explode(i, now)
			i = 0
		} else {
			i++
		}
	}
	alive := 0
	var winner uint64
	for i := range g.Players {
		p := &g.Players[i]
		if p.Alive && now.Before(g.Flames[p.Pos.Y][p.Pos.X]) {
			p.Alive = false
		}
		if p.Alive {
			alive++
			winner = p.ID
		}
	}
	if alive <= 1 {
		g.Over = true
		g.Winner = winner
	} else if now.Sub(g.Started) >= 3*time.Minute {
		g.Over = true
		g.Winner = 0
	}
}
func (g *Game) ignite(p Pos, now time.Time) {
	g.Flames[p.Y][p.X] = now.Add(500 * time.Millisecond)
	g.Powers[p.Y][p.X] = None
	if i := g.bombAt(p); i >= 0 {
		g.explode(i, now)
	}
}
func (g *Game) explode(i int, now time.Time) {
	b := g.Bombs[i]
	copy(g.Bombs[i:g.BombCount-1], g.Bombs[i+1:g.BombCount])
	g.BombCount--
	g.Bombs[g.BombCount] = Bomb{}
	g.ignite(b.Pos, now)
	for _, d := range []Pos{{0, -1}, {1, 0}, {0, 1}, {-1, 0}} {
		for n := 1; n <= b.Range; n++ {
			p := Pos{b.Pos.X + d.X*n, b.Pos.Y + d.Y*n}
			if p.X < 0 || p.Y < 0 || p.X >= Width || p.Y >= Height || g.Tiles[p.Y][p.X] == Wall {
				break
			}
			block := g.Tiles[p.Y][p.X] == Block
			g.ignite(p, now)
			if block {
				g.Tiles[p.Y][p.X] = Floor
				if g.rng.Intn(100) < 30 {
					g.Powers[p.Y][p.X] = Power(1 + g.rng.Intn(3))
				}
				break
			}
		}
	}
}
