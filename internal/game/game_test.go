package game

import (
	"reflect"
	"testing"
	"time"
)

var epoch = time.Unix(1000, 0)

func arena() *Game {
	g := New([]uint64{1, 2, 3, 4}, epoch, 42)
	for y := 1; y < Height-1; y++ {
		for x := 1; x < Width-1; x++ {
			g.Tiles[y][x] = Floor
		}
	}
	return g
}
func TestMovementAndBombs(t *testing.T) {
	g := arena()
	if g.Move(1, -1, 0, epoch) {
		t.Fatal("walked into wall")
	}
	if !g.Place(1, epoch) || g.Place(1, epoch) {
		t.Fatal("bomb placement/limit")
	}
	if !g.Move(1, 1, 0, epoch) {
		t.Fatal("cannot leave own bomb")
	}
	if g.Move(1, 1, 0, epoch.Add(99*time.Millisecond)) {
		t.Fatal("cooldown ignored")
	}
	if g.Move(1, -1, 0, epoch.Add(100*time.Millisecond)) {
		t.Fatal("reentered bomb")
	}
	if g.Place(1, epoch) {
		t.Fatal("capacity ignored")
	}
	g.Tiles[1][3] = Block
	if g.Move(1, 1, 0, epoch.Add(time.Second)) {
		t.Fatal("walked through block")
	}
	g.Tiles[1][3] = Floor
	g.Players[1].Pos = Pos{3, 1}
	if !g.Move(1, 1, 0, epoch.Add(time.Second)) {
		t.Fatal("cannot enter another player's tile")
	}
	g.Tick(epoch.Add(1500 * time.Millisecond))
	if g.Players[0].Pos != (Pos{3, 1}) || g.Players[1].Pos != (Pos{3, 1}) || !g.Players[0].Alive || !g.Players[1].Alive {
		t.Fatal("players cannot stay on the same tile")
	}
	if !g.Move(1, 1, 0, epoch.Add(1500*time.Millisecond)) || !g.Move(2, -1, 0, epoch.Add(1500*time.Millisecond)) {
		t.Fatal("players cannot move past each other")
	}
	if g.Players[0].Pos != (Pos{4, 1}) || g.Players[1].Pos != (Pos{2, 1}) {
		t.Fatal("players did not pass each other")
	}
	if g.Move(1, 1, 1, epoch.Add(1700*time.Millisecond)) {
		t.Fatal("diagonal move")
	}
}
func TestBlastBlockingAndFlameLifetime(t *testing.T) {
	g := arena()
	g.Players[0].Pos = Pos{5, 5}
	g.Players[0].Range = 5
	g.Tiles[5][6] = Block
	g.Tiles[4][5] = Wall
	g.Place(1, epoch)
	g.Players[0].Pos = Pos{1, 1}
	g.Tick(epoch.Add(1999 * time.Millisecond))
	if g.BombCount != 1 {
		t.Fatal("early detonation")
	}
	now := epoch.Add(2 * time.Second)
	g.Tick(now)
	if g.Tiles[5][6] != Floor || !g.Flames[5][6].After(now) {
		t.Fatal("block not destroyed")
	}
	if !g.Flames[5][7].IsZero() || !g.Flames[4][5].IsZero() || !g.Flames[3][5].IsZero() {
		t.Fatal("blast passed obstacle")
	}
	g.Tick(now.Add(499 * time.Millisecond))
	if g.Flames[5][5].IsZero() {
		t.Fatal("flame ended early")
	}
	g.Tick(now.Add(500 * time.Millisecond))
	if !g.Flames[5][5].IsZero() {
		t.Fatal("flame lingered")
	}
}
func TestChainAndSimultaneousDeaths(t *testing.T) {
	g := arena()
	g.Players[0].Pos = Pos{3, 3}
	g.Players[1].Pos = Pos{5, 3}
	g.Players[2].Alive = false
	g.Players[3].Alive = false
	g.Place(1, epoch)
	g.Place(2, epoch.Add(time.Second))
	g.Tick(epoch.Add(2 * time.Second))
	if g.BombCount != 0 || !g.Over || g.Winner != 0 || g.Players[0].Alive || g.Players[1].Alive {
		t.Fatal("chain must kill both and draw")
	}
}
func TestFlamesKillOnEntryAndOwner(t *testing.T) {
	g := arena()
	g.Flames[1][2] = epoch.Add(time.Second)
	if !g.Move(1, 1, 0, epoch) || g.Players[0].Alive {
		t.Fatal("flames did not kill on entry")
	}
	g = arena()
	g.Place(1, epoch)
	g.Tick(epoch.Add(2 * time.Second))
	if g.Players[0].Alive {
		t.Fatal("owner immune")
	}
}
func TestPowerUpsAndCaps(t *testing.T) {
	for _, power := range []Power{Capacity, Range, Speed} {
		g := arena()
		for n := 0; n < 12; n++ {
			g.Players[0].Pos = Pos{1, 1}
			g.Powers[1][2] = power
			if !g.Move(1, 1, 0, epoch.Add(time.Duration(n)*time.Second)) {
				t.Fatal("pickup movement")
			}
			if g.Powers[1][2] != None {
				t.Fatal("pickup not consumed")
			}
		}
		p := g.Players[0]
		if power == Capacity && p.Capacity != 5 || power == Range && p.Range != 8 || power == Speed && p.Cooldown != 25*time.Millisecond {
			t.Fatalf("wrong upgrade cap: %+v", p)
		}
	}
	g := New([]uint64{1, 2}, epoch, 1)
	if g.Players[0].Capacity != 1 || g.Players[0].Range != 2 || g.Players[0].Cooldown != 100*time.Millisecond {
		t.Fatal("round defaults")
	}
}
func TestCapacityCountsOnlyOwnedLiveBombs(t *testing.T) {
	g := arena()
	g.Players[0].Capacity = 5
	for n := 0; n < 5; n++ {
		g.Players[0].Pos = Pos{1 + n*2, 3}
		if !g.Place(1, epoch) {
			t.Fatal("capacity five unavailable")
		}
	}
	g.Players[0].Pos = Pos{11, 3}
	if g.Place(1, epoch) {
		t.Fatal("exceeded five")
	}
	if !g.Place(2, epoch) {
		t.Fatal("other owners count against capacity")
	}
	g.Players[0].Pos = Pos{2, 1}
	g.Players[1].Pos = Pos{12, 1}
	g.Tick(epoch.Add(2 * time.Second))
	if !g.Place(1, epoch.Add(3*time.Second)) {
		t.Fatal("capacity not released")
	}
}
func TestEliminationAndTimeLimit(t *testing.T) {
	g := arena()
	g.Place(1, epoch)
	g.Remove(1)
	g.Tick(epoch.Add(2 * time.Second))
	if g.BombCount != 0 || g.Flames[1][1].IsZero() {
		t.Fatal("departed player's bomb lost")
	}
	g = arena()
	g.Tick(epoch.Add(3*time.Minute - time.Nanosecond))
	if g.Over {
		t.Fatal("early timeout")
	}
	g.Tick(epoch.Add(3 * time.Minute))
	if !g.Over || g.Winner != 0 {
		t.Fatal("timeout must draw")
	}
	g = arena()
	g.Remove(1)
	g.Remove(2)
	g.Remove(3)
	g.Tick(epoch)
	if !g.Over || g.Winner != 4 {
		t.Fatal("last survivor")
	}
}
func TestMapSafetySymmetryAndConnectivity(t *testing.T) {
	for seed := int64(0); seed < 100; seed++ {
		g := New([]uint64{1, 2, 3, 4}, epoch, seed)
		for y := 0; y < Height; y++ {
			for x := 0; x < Width; x++ {
				tile := g.Tiles[y][x]
				if tile != g.Tiles[y][Width-1-x] || tile != g.Tiles[Height-1-y][x] {
					t.Fatal("asymmetric map")
				}
				if (x == 0 || y == 0 || x == Width-1 || y == Height-1 || x%2 == 0 && y%2 == 0) && tile != Wall {
					t.Fatal("missing wall")
				}
			}
		}
		for _, p := range Spawns {
			safe := 0
			for _, d := range []Pos{{0, 0}, {1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				if g.Tiles[p.Y+d.Y][p.X+d.X] == Floor {
					safe++
				}
			}
			if safe != 3 {
				t.Fatal("unsafe spawn")
			}
		}
		seen := map[Pos]bool{Spawns[0]: true}
		queue := []Pos{Spawns[0]}
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			for _, d := range []Pos{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				q := Pos{p.X + d.X, p.Y + d.Y}
				if !seen[q] && g.Tiles[q.Y][q.X] != Wall {
					seen[q] = true
					queue = append(queue, q)
				}
			}
		}
		for _, p := range Spawns {
			if !seen[p] {
				t.Fatal("spawn inaccessible after clearing blocks")
			}
		}
	}
}
func TestSeededRoundsAndDrops(t *testing.T) {
	a, b := arena(), arena()
	counts := [4]int{}
	// Exercise seeded drop decisions over many destroyed blocks.
	for n := 0; n < 10000; n++ {
		for _, g := range []*Game{a, b} {
			g.Tiles[3][4] = Block
			g.Bombs[0] = Bomb{Pos: Pos{3, 3}, Range: 1, Due: epoch}
			g.BombCount = 1
			g.explode(0, epoch)
		}
		counts[a.Powers[3][4]]++
		if !reflect.DeepEqual(a.View, b.View) {
			t.Fatal("seeded replay diverged")
		}
	}
	drops := 10000 - counts[0]
	if drops < 2800 || drops > 3200 {
		t.Fatalf("drop rate: %d", drops)
	}
	for _, n := range counts[1:] {
		if n < 850 || n > 1150 {
			t.Fatalf("unbalanced upgrades: %v", counts)
		}
	}
	if reflect.DeepEqual(New([]uint64{1, 2}, epoch, 1).Tiles, New([]uint64{1, 2}, epoch, 2).Tiles) {
		t.Fatal("seed has no effect")
	}
}

func TestDeterministicRoundReplay(t *testing.T) {
	a := New([]uint64{1, 2, 3, 4}, epoch, 12345)
	b := New([]uint64{1, 2, 3, 4}, epoch, 12345)
	if a.View != b.View {
		t.Fatal("same seed produced different starting worlds")
	}
	for tick := 0; tick <= 3600; tick++ {
		now := epoch.Add(time.Duration(tick) * 50 * time.Millisecond)
		for _, g := range []*Game{a, b} {
			for id := uint64(1); id <= 4; id++ {
				if tick%40 == 0 {
					g.Place(id, now)
				}
				direction := []Pos{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}[(tick/3+int(id))%4]
				g.Move(id, direction.X, direction.Y, now)
			}
			g.Tick(now)
		}
		if a.View != b.View {
			t.Fatalf("replay diverged at tick %d", tick)
		}
		if a.Over {
			return
		}
	}
	t.Fatal("replayed round did not finish")
}
