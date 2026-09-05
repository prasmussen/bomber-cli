// Package game implements deterministic Bomberman rules. All time comes from callers.
package game

import (
	"math/rand"
	"time"
)

const (
	Width           = 15
	Height          = 13
	MaxPlayers      = 4
	MaxCapacity     = 5
	MaxRange        = 8
	RoundDuration   = 3 * time.Minute
	BombFuse        = 2 * time.Second
	FlameDuration   = 500 * time.Millisecond
	InitialCooldown = 100 * time.Millisecond
	MinCooldown     = 25 * time.Millisecond
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

// Spawns returns a fresh value so callers cannot change future rounds.
func Spawns() [MaxPlayers]Pos {
	return [MaxPlayers]Pos{{1, 1}, {Width - 2, Height - 2}, {Width - 2, 1}, {1, Height - 2}}
}

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
	Players      [MaxPlayers]Player
	Bombs        [MaxPlayers * MaxCapacity]Bomb
	BombCount    int
	Started, Now time.Time
	Over         bool
	Winner       uint64 // Zero denotes a draw.
}

// Game owns a round and its random stream. It must only be mutated by its
// owning room actor; publish View copies to other goroutines.
type Game struct {
	View
	rng *rand.Rand
}

func New(ids []uint64, now time.Time, seed int64) *Game {
	rng := rand.New(rand.NewSource(seed))
	return &Game{
		Tiles:   initialTiles(sampleBlocks(rng)),
		Players: initialPlayers(ids),
		Started: now,
		Now:     now,
		rng:     rng,
	}
}

func (g *Game) Move(id uint64, dx, dy int, now time.Time) bool {
	next, moved := movedView(g.View, id, dx, dy, now)
	g.View = next
	return moved
}

func (g *Game) Place(id uint64, now time.Time) bool {
	next, placed := viewWithBomb(g.View, id, now)
	g.View = next
	return placed
}

func (g *Game) Remove(id uint64) {
	g.Players = playersWithout(g.Players, id)
}

func (g *Game) Tick(now time.Time) {
	if g.Over {
		return
	}
	g.Now = now
	g.Flames = activeFlames(g.Flames, now)
	g.detonateTriggeredBombs(now)
	g.Players = survivingPlayers(g.Players, g.Flames, now)
	g.Over, g.Winner = outcome(g.Players, now.Sub(g.Started))
}

func (g *Game) detonateTriggeredBombs(now time.Time) {
	for i := 0; i < g.BombCount; {
		if !now.Before(g.Bombs[i].Due) || now.Before(g.Flames[g.Bombs[i].Pos.Y][g.Bombs[i].Pos.X]) {
			g.explode(i, now)
			i = 0
		} else {
			i++
		}
	}
}

func (g *Game) ignite(p Pos, now time.Time) {
	g.Flames[p.Y][p.X] = now.Add(FlameDuration)
	g.Powers[p.Y][p.X] = None
	if i := bombIndex(g.Bombs[:g.BombCount], p); i >= 0 {
		g.explode(i, now)
	}
}

func (g *Game) explode(index int, now time.Time) {
	// Remove before igniting so recursive chain reactions cannot revisit this bomb.
	bomb := g.removeBomb(index)
	g.ignite(bomb.Pos, now)
	for _, direction := range []Pos{{0, -1}, {1, 0}, {0, 1}, {-1, 0}} {
		g.propagateBlast(bomb, direction, now)
	}
}

func (g *Game) removeBomb(index int) Bomb {
	bomb := g.Bombs[index]
	copy(g.Bombs[index:g.BombCount-1], g.Bombs[index+1:g.BombCount])
	g.BombCount--
	g.Bombs[g.BombCount] = Bomb{}
	return bomb
}

func (g *Game) propagateBlast(bomb Bomb, direction Pos, now time.Time) {
	for distance := 1; distance <= bomb.Range; distance++ {
		position := Pos{bomb.Pos.X + direction.X*distance, bomb.Pos.Y + direction.Y*distance}
		if !insideBoard(position) || g.Tiles[position.Y][position.X] == Wall {
			return
		}
		block := g.Tiles[position.Y][position.X] == Block
		g.ignite(position, now)
		if block {
			g.destroyBlock(position)
			return
		}
	}
}

func (g *Game) destroyBlock(position Pos) {
	g.Tiles[position.Y][position.X] = Floor
	if g.rng.Intn(100) < 30 {
		g.Powers[position.Y][position.X] = Power(1 + g.rng.Intn(3))
	}
}
