package game

import "time"

func insideBoard(position Pos) bool {
	return position.X >= 0 && position.Y >= 0 && position.X < Width && position.Y < Height
}

// Compare components directly: taking abs of the minimum int overflows.
func cardinalStep(dx, dy int) bool {
	return dx == 0 && (dy == -1 || dy == 1) || dy == 0 && (dx == -1 || dx == 1)
}

// upgraded returns the new player value without modifying its input.
func upgraded(p Player, power Power) Player {
	switch power {
	case Capacity:
		p.Capacity = min(p.Capacity+1, MaxCapacity)
	case Range:
		p.Range = min(p.Range+1, MaxRange)
	case Speed:
		p.Cooldown = max(p.Cooldown-MinCooldown, MinCooldown)
	}
	return p
}

// Elimination takes precedence over the round deadline, including simultaneous deaths.
func outcome(players [MaxPlayers]Player, elapsed time.Duration) (bool, uint64) {
	alive := 0
	var winner uint64
	for _, p := range players {
		if p.Alive {
			alive++
			winner = p.ID
		}
	}
	if alive <= 1 {
		return true, winner
	}
	return elapsed >= RoundDuration, 0
}

func playerIndex(players [MaxPlayers]Player, id uint64) int {
	for i, player := range players {
		if id != 0 && player.ID == id {
			return i
		}
	}
	return -1
}

func bombIndex(bombs []Bomb, position Pos) int {
	for i, bomb := range bombs {
		if bomb.Pos == position {
			return i
		}
	}
	return -1
}

func ownedBombCount(bombs []Bomb, id uint64) int {
	count := 0
	for _, bomb := range bombs {
		if bomb.Owner == id {
			count++
		}
	}
	return count
}

func movedView(view View, id uint64, dx, dy int, now time.Time) (View, bool) {
	index := playerIndex(view.Players, id)
	if view.Over || index < 0 || !cardinalStep(dx, dy) {
		return view, false
	}
	player := view.Players[index]
	destination := Pos{player.Pos.X + dx, player.Pos.Y + dy}
	if !player.Alive || now.Before(player.NextMove) || !insideBoard(destination) {
		return view, false
	}
	if view.Tiles[destination.Y][destination.X] != Floor || bombIndex(view.Bombs[:view.BombCount], destination) >= 0 {
		return view, false
	}
	player.Pos = destination
	player.NextMove = now.Add(player.Cooldown)
	player.Alive = !now.Before(view.Flames[destination.Y][destination.X])
	if player.Alive {
		player = upgraded(player, view.Powers[destination.Y][destination.X])
		view.Powers[destination.Y][destination.X] = None
	}
	view.Players[index] = player
	return view, true
}

func viewWithBomb(view View, id uint64, now time.Time) (View, bool) {
	index := playerIndex(view.Players, id)
	if view.Over || index < 0 || view.BombCount == len(view.Bombs) {
		return view, false
	}
	player := view.Players[index]
	bombs := view.Bombs[:view.BombCount]
	if !player.Alive || bombIndex(bombs, player.Pos) >= 0 || ownedBombCount(bombs, id) >= player.Capacity {
		return view, false
	}
	view.Bombs[view.BombCount] = Bomb{player.Pos, id, player.Range, now.Add(BombFuse)}
	view.BombCount++
	return view, true
}

func playersWithout(players [MaxPlayers]Player, id uint64) [MaxPlayers]Player {
	if index := playerIndex(players, id); index >= 0 {
		players[index].Alive = false
	}
	return players
}

func activeFlames(flames [Height][Width]time.Time, now time.Time) [Height][Width]time.Time {
	for y := range flames {
		for x, until := range flames[y] {
			if !now.Before(until) {
				flames[y][x] = time.Time{}
			}
		}
	}
	return flames
}

func survivingPlayers(players [MaxPlayers]Player, flames [Height][Width]time.Time, now time.Time) [MaxPlayers]Player {
	for i, player := range players {
		if player.Alive && now.Before(flames[player.Pos.Y][player.Pos.X]) {
			players[i].Alive = false
		}
	}
	return players
}
