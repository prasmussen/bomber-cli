// Package ui presents immutable room snapshots using Bubble Tea.
package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"bomber-cli/internal/game"
	"bomber-cli/internal/room"
	tea "github.com/charmbracelet/bubbletea"
)

type pulse time.Time
type Model struct {
	Hub           *room.Hub
	Session       *room.Session
	Room          *room.Room
	Snapshot      room.Snapshot
	Width, Height int
	Color         bool
	selected      int
	rooms         []room.Snapshot
	notice        string
	now           time.Time
}

func New(h *room.Hub, s *room.Session, w, height int, color bool) Model {
	return Model{Hub: h, Session: s, Width: w, Height: height, Color: color, now: time.Now(), rooms: h.List()}
}
func tick() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg { return pulse(t) })
}
func (m Model) Init() tea.Cmd { return tick() }
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case room.Snapshot:
		if m.Room != nil && v.ID == m.Room.ID {
			m.Snapshot = v
		}
	case tea.WindowSizeMsg:
		m.Width = v.Width
		m.Height = v.Height
	case pulse:
		m.now = time.Time(v)
		if m.Room == nil {
			m.rooms = m.Hub.List()
			if m.selected >= len(m.rooms)+2 {
				m.selected = 0
			}
		}
		return m, tick()
	case tea.KeyMsg:
		key := v.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if key == "esc" && m.Room != nil {
			m.Hub.Leave(m.Session)
			m.Room = nil
			m.rooms = m.Hub.List()
			m.selected = 0
			return m, nil
		}
		if m.Width < 60 || m.Height < 24 {
			return m, nil
		}
		if m.Room == nil {
			switch key {
			case "up", "w":
				m.selected = (m.selected + len(m.rooms) + 1) % (len(m.rooms) + 2)
			case "down", "s":
				m.selected = (m.selected + 1) % (len(m.rooms) + 2)
			case "enter":
				var id uint64
				if m.selected >= 2 {
					id = m.rooms[m.selected-2].ID
				}
				r, err := m.Hub.Join(m.Session, id, m.selected == 1)
				if err != nil {
					m.notice = err.Error()
				} else {
					m.Room = r
					m.Snapshot = r.Snapshot()
					m.notice = ""
				}
			}
		} else {
			action := room.Action(255)
			switch key {
			case "enter":
				action = room.Ready
			case "up", "w":
				action = room.Up
			case "down", "s":
				action = room.Down
			case "left", "a":
				action = room.Left
			case "right", "d":
				action = room.Right
			case " ":
				action = room.Bomb
			}
			if action != 255 {
				m.Room.Submit(m.Session.ID, action)
			}
		}
	}
	return m, nil
}
func (m Model) paint(s string, color int) string {
	if !m.Color {
		return s
	}
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, s)
}
func (m Model) View() string {
	if m.Width < 60 || m.Height < 24 {
		return fmt.Sprintf("Resize terminal to at least 60 x 24.\nCurrent: %d x %d\nMatch continues. Esc: leave | Ctrl-C: disconnect", m.Width, m.Height)
	}
	var b strings.Builder
	b.WriteString(m.paint("BOMBER / SSH", 36) + "   " + m.Session.Name + "\n\n")
	if m.Room == nil {
		b.WriteString("LOBBY - choose with arrows / W S, then Enter\n\n")
		options := []string{"Quick join", "Create room"}
		for _, r := range m.rooms {
			options = append(options, fmt.Sprintf("Room %d  %d/4  %s", r.ID, r.Count, r.Phase))
		}
		// Scroll the room list to keep the selected item visible at 60 x 24.
		limit := m.Height - 10
		start := 0
		if m.selected >= limit {
			start = m.selected - limit + 1
		}
		for i := start; i < len(options) && i < start+limit; i++ {
			prefix := "  "
			if i == m.selected {
				prefix = "> "
			}
			b.WriteString(prefix + options[i] + "\n")
		}
		b.WriteString("\n" + m.notice + "\nEnter: join | Ctrl-C: disconnect\n")
		return b.String()
	}
	s := m.Snapshot
	fmt.Fprintf(&b, "ROOM %d  %s", s.ID, strings.ToUpper(string(s.Phase)))
	if s.Phase == room.Countdown {
		fmt.Fprintf(&b, "  starts in %ds", seconds(s.Deadline.Sub(m.now)))
	}
	if s.Phase == room.Playing {
		fmt.Fprintf(&b, "  %ds left", seconds(3*time.Minute-m.now.Sub(s.Game.Started)))
	}
	b.WriteByte('\n')
	if s.Phase == room.Playing || s.Phase == room.Result {
		for y := 0; y < game.Height; y++ {
			for x := 0; x < game.Width; x++ {
				p := game.Pos{X: x, Y: y}
				tile := "  "
				c := 37
				switch s.Game.Tiles[y][x] {
				case game.Wall:
					tile = "##"
					c = 34
				case game.Block:
					tile = "[]"
					c = 33
				}
				switch s.Game.Powers[y][x] {
				case game.Capacity:
					tile = "B+"
					c = 32
				case game.Range:
					tile = "R+"
					c = 32
				case game.Speed:
					tile = "S+"
					c = 32
				}
				for i := 0; i < s.Game.BombCount; i++ {
					bomb := s.Game.Bombs[i]
					if bomb.Pos == p {
						// Pulse at a steady rhythm independent of the fuse.
						tile, c = "o*", 35
						if s.Game.Now.Sub(s.Game.Started)/(250*time.Millisecond)%2 == 1 {
							tile, c = "O*", 95
						}
					}
				}
				for i, player := range s.Game.Players {
					if player.ID != 0 && player.Alive && player.Pos == p {
						tile = fmt.Sprintf("P%d", i+1)
						c = []int{36, 32, 35, 33}[i]
					}
				}
				if s.Game.Now.Before(s.Game.Flames[y][x]) {
					tile = "**"
					c = 31
				}
				b.WriteString(m.paint(tile, c))
			}
			b.WriteByte('\n')
		}
	}
	for i, member := range s.Members {
		if member.ID == 0 {
			continue
		}
		status := "not ready"
		number := i + 1
		if member.Ready {
			status = "ready"
		}
		if s.Phase == room.Playing || s.Phase == room.Result {
			status = "out"
			for j, p := range s.Game.Players {
				if p.ID == member.ID {
					number = j + 1
					if p.Alive {
						status = "alive"
					}
					if member.ID == m.Session.ID {
						status += fmt.Sprintf(" B%d R%d %dms", p.Capacity, p.Range, p.Cooldown.Milliseconds())
					}
				}
			}
		}
		fmt.Fprintf(&b, "P%d %-20s %d wins  %s\n", number, member.Name, member.Score, status)
	}
	if s.Phase == room.Waiting || s.Phase == room.Countdown {
		b.WriteString("\nEnter: toggle ready (2-4 players, all must be ready)\n")
	}
	if s.Message != "" {
		b.WriteString(s.Message + "\n")
	}
	b.WriteString("WASD/arrows: move | Space: bomb | Enter: ready\nEsc: leave | Ctrl-C: disconnect")
	return b.String()
}
func seconds(d time.Duration) int { return max(0, int(math.Ceil(d.Seconds()))) }
