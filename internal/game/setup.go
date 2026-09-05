package game

import "math/rand"

type blockPattern [Height/2 + 1][Width/2 + 1]bool

func sampleBlocks(rng *rand.Rand) blockPattern {
	var blocks blockPattern
	for y := 1; y <= Height/2; y++ {
		for x := 1; x <= Width/2; x++ {
			blocks[y][x] = !wallPosition(Pos{x, y}) && rng.Intn(100) < 65
		}
	}
	return blocks
}

func initialTiles(blocks blockPattern) [Height][Width]Tile {
	var tiles [Height][Width]Tile
	for y := range Height {
		for x := range Width {
			tiles[y][x] = initialTile(Pos{x, y}, blocks)
		}
	}
	return tiles
}

func initialTile(position Pos, blocks blockPattern) Tile {
	x := min(position.X, Width-1-position.X)
	y := min(position.Y, Height-1-position.Y)
	switch {
	case wallPosition(position):
		return Wall
	case spawnExit(position):
		return Floor
	case blocks[y][x]:
		return Block
	default:
		return Floor
	}
}

func spawnExit(position Pos) bool {
	for _, spawn := range Spawns() {
		if position == spawn || cardinalStep(position.X-spawn.X, position.Y-spawn.Y) {
			return true
		}
	}
	return false
}

func wallPosition(position Pos) bool {
	return position.X == 0 || position.Y == 0 || position.X == Width-1 || position.Y == Height-1 || position.X%2 == 0 && position.Y%2 == 0
}

func initialPlayers(ids []uint64) [MaxPlayers]Player {
	var players [MaxPlayers]Player
	spawns := Spawns()
	for i, id := range ids[:min(len(ids), MaxPlayers)] {
		players[i] = Player{ID: id, Pos: spawns[i], Alive: true, Capacity: 1, Range: 2, Cooldown: InitialCooldown}
	}
	return players
}
