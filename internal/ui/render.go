package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"bomber-cli/internal/game"
	"bomber-cli/internal/room"
)

func paint(s string, color int, enabled bool) string {
	if !enabled {
		return s
	}
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, s)
}

func (m Model) View() string {
	if m.Width < 60 || m.Height < 24 {
		return fmt.Sprintf("Resize terminal to at least 60 x 24.\nCurrent: %d x %d\nMatch continues. Esc: leave | Ctrl-C: disconnect", m.Width, m.Height)
	}
	header := paint("BOMBER / SSH", 36, m.Color) + "   " + m.Session.Name + "\n\n"
	if m.Room == nil {
		return header + m.renderLobby()
	}
	return header + m.renderRoom()
}

func (m Model) renderLobby() string {
	return "LOBBY - choose with arrows / W S, then Enter\n\n" +
		visibleLobbyOptions(m.rooms, m.selected, m.Height-10) +
		"\n" + m.notice + "\nEnter: join | Ctrl-C: disconnect\n"
}

func visibleLobbyOptions(rooms []room.Snapshot, selected, limit int) string {
	options := []string{"Quick join", "Create room"}
	for _, snapshot := range rooms {
		options = append(options, fmt.Sprintf("Room %d  %d/4  %s", snapshot.ID, snapshot.Count, snapshot.Phase))
	}
	start := max(0, selected-limit+1)
	var lines []string
	for i := start; i < len(options) && i < start+limit; i++ {
		prefix := "  "
		if i == selected {
			prefix = "> "
		}
		lines = append(lines, prefix+options[i]+"\n")
	}
	return strings.Join(lines, "")
}

func (m Model) renderRoom() string {
	return roomHeading(m.Snapshot, m.now) +
		roomBoard(m.Snapshot, m.Color) +
		m.renderMembers() +
		roomInstructions(m.Snapshot)
}

func roomHeading(snapshot room.Snapshot, now time.Time) string {
	heading := fmt.Sprintf("ROOM %d  %s", snapshot.ID, strings.ToUpper(string(snapshot.Phase)))
	switch snapshot.Phase {
	case room.Countdown:
		return heading + fmt.Sprintf("  starts in %ds\n", seconds(snapshot.Deadline.Sub(now)))
	case room.Playing:
		return heading + fmt.Sprintf("  %ds left\n", seconds(game.RoundDuration-now.Sub(snapshot.Game.Started)))
	default:
		return heading + "\n"
	}
}

func roomBoard(snapshot room.Snapshot, color bool) string {
	if snapshot.Phase == room.Playing || snapshot.Phase == room.Result {
		return renderBoard(snapshot.Game, color)
	}
	return ""
}

func roomInstructions(snapshot room.Snapshot) string {
	ready := ""
	if snapshot.Phase == room.Waiting || snapshot.Phase == room.Countdown {
		ready = "\nEnter: toggle ready (2-4 players, all must be ready)\n"
	}
	message := ""
	if snapshot.Message != "" {
		message = snapshot.Message + "\n"
	}
	return ready + message + "WASD/arrows: move | Space: bomb | Enter: ready\nEsc: leave | Ctrl-C: disconnect"
}

func (m Model) renderMembers() string {
	var lines []string
	for i, member := range m.Snapshot.Members {
		if member.ID == 0 {
			continue
		}
		number, status := m.memberStatus(i, member)
		lines = append(lines, fmt.Sprintf("P%d %-20s %d wins  %s\n", number, member.Name, member.Score, status))
	}
	return strings.Join(lines, "")
}

func (m Model) memberStatus(index int, member room.Member) (int, string) {
	s := m.Snapshot
	status := "not ready"
	number := index + 1
	if member.Ready {
		status = "ready"
	}
	if s.Phase != room.Playing && s.Phase != room.Result {
		return number, status
	}
	status = "out"
	for i, player := range s.Game.Players {
		if player.ID != member.ID {
			continue
		}
		number = i + 1
		if player.Alive {
			status = "alive"
		}
		if member.ID == m.Session.ID {
			status += fmt.Sprintf(" B%d R%d %dms", player.Capacity, player.Range, player.Cooldown.Milliseconds())
		}
	}
	return number, status
}

func seconds(d time.Duration) int { return max(0, int(math.Ceil(d.Seconds()))) }

// renderBoard projects a snapshot into terminal cells without changing game state.
func renderBoard(view game.View, color bool) string {
	var b strings.Builder
	for y := range game.Height {
		for x := range game.Width {
			tile, colorCode := boardCell(&view, game.Pos{X: x, Y: y})
			b.WriteString(paint(tile, colorCode, color))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func boardCell(view *game.View, position game.Pos) (string, int) {
	if view.Now.Before(view.Flames[position.Y][position.X]) {
		return "**", 31
	}
	// Later player slots take precedence when players share a tile.
	for i := len(view.Players) - 1; i >= 0; i-- {
		player := view.Players[i]
		if player.ID != 0 && player.Alive && player.Pos == position {
			return fmt.Sprintf("P%d", i+1), [...]int{36, 32, 35, 33}[i]
		}
	}
	for _, bomb := range view.Bombs[:view.BombCount] {
		if bomb.Pos == position {
			return pulsingBombCell(view.Now.Sub(view.Started))
		}
	}
	return terrainCell(view.Tiles[position.Y][position.X], view.Powers[position.Y][position.X])
}

func pulsingBombCell(elapsed time.Duration) (string, int) {
	if elapsed/(250*time.Millisecond)%2 == 1 {
		return "O*", 95
	}
	return "o*", 35
}

func terrainCell(tile game.Tile, power game.Power) (string, int) {
	switch power {
	case game.Capacity:
		return "B+", 32
	case game.Range:
		return "R+", 32
	case game.Speed:
		return "S+", 32
	}
	switch tile {
	case game.Wall:
		return "##", 34
	case game.Block:
		return "[]", 33
	default:
		return "  ", 37
	}
}
