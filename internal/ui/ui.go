// Package ui presents immutable room snapshots using Bubble Tea.
package ui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bomber-cli/internal/room"
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
		return m.handleKey(v.String())
	}
	return m, nil
}

func (m Model) handleKey(key string) (tea.Model, tea.Cmd) {
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if key == "esc" && m.Room != nil {
		return m.leaveRoom(), nil
	}
	if m.Width < 60 || m.Height < 24 {
		return m, nil
	}
	if m.Room == nil {
		return m.handleLobbyKey(key), nil
	}
	if action, ok := actionForKey(key); ok {
		m.Room.Submit(m.Session.ID, action)
	}
	return m, nil
}

func (m Model) leaveRoom() Model {
	m.Hub.Leave(m.Session)
	m.Room = nil
	m.rooms = m.Hub.List()
	m.selected = 0
	return m
}

func (m Model) handleLobbyKey(key string) Model {
	switch key {
	case "up", "w":
		m.selected = (m.selected + len(m.rooms) + 1) % (len(m.rooms) + 2)
	case "down", "s":
		m.selected = (m.selected + 1) % (len(m.rooms) + 2)
	case "enter":
		return m.joinSelectedRoom()
	}
	return m
}

func (m Model) joinSelectedRoom() Model {
	var id uint64
	if m.selected >= 2 {
		id = m.rooms[m.selected-2].ID
	}
	r, err := m.Hub.Join(m.Session, id, m.selected == 1)
	if err != nil {
		m.notice = err.Error()
		return m
	}
	m.Room = r
	m.Snapshot = r.Snapshot()
	m.notice = ""
	return m
}

func actionForKey(key string) (room.Action, bool) {
	switch key {
	case "enter":
		return room.Ready, true
	case "up", "w":
		return room.Up, true
	case "down", "s":
		return room.Down, true
	case "left", "a":
		return room.Left, true
	case "right", "d":
		return room.Right, true
	case " ":
		return room.Bomb, true
	default:
		return 0, false
	}
}
