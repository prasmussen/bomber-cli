package room

import (
	"errors"
	"time"

	"bomber-cli/internal/game"
)

func allReady(s Snapshot) bool {
	if s.Count < 2 {
		return false
	}
	for _, m := range s.Members {
		if m.ID != 0 && !m.Ready {
			return false
		}
	}
	return true
}

func playerIDs(s Snapshot) []uint64 {
	ids := make([]uint64, 0, s.Count)
	for _, m := range s.Members {
		if m.ID != 0 {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func memberIndex(members [game.MaxPlayers]Member, id uint64) int {
	for i, member := range members {
		if member.ID == id {
			return i
		}
	}
	return -1
}

func withMember(snapshot Snapshot, member Member) (Snapshot, error) {
	if snapshot.Phase != Waiting && snapshot.Phase != Countdown {
		return snapshot, errors.New("round in progress")
	}
	if snapshot.Count >= game.MaxPlayers {
		return snapshot, errors.New("room is full")
	}
	if index := memberIndex(snapshot.Members, 0); index >= 0 {
		snapshot.Members[index] = member
		snapshot.Count++
	}
	return snapshot, nil
}

func withoutMember(snapshot Snapshot, id uint64) Snapshot {
	if index := memberIndex(snapshot.Members, id); index >= 0 {
		snapshot.Members[index] = Member{}
		snapshot.Count--
	}
	return snapshot
}

func withoutReadiness(snapshot Snapshot) Snapshot {
	for i := range snapshot.Members {
		snapshot.Members[i].Ready = false
	}
	return snapshot
}

func withoutCountdown(snapshot Snapshot) Snapshot {
	if snapshot.Phase == Countdown {
		snapshot.Phase = Waiting
		snapshot.Deadline = time.Time{}
	}
	return snapshot
}

func withReadinessToggled(snapshot Snapshot, index int) Snapshot {
	snapshot.Members[index].Ready = !snapshot.Members[index].Ready
	return withoutCountdown(snapshot)
}

func countdownSnapshot(snapshot Snapshot, now time.Time) Snapshot {
	snapshot.Phase = Countdown
	snapshot.Deadline = now.Add(3 * time.Second)
	snapshot.Message = ""
	return snapshot
}

func resultSnapshot(snapshot Snapshot, winner uint64, now time.Time) Snapshot {
	snapshot.Phase = Result
	snapshot.Deadline = now.Add(3 * time.Second)
	snapshot.Message = "Draw!"
	for i, member := range snapshot.Members {
		if member.ID != 0 && member.ID == winner {
			snapshot.Members[i].Score++
			snapshot.Message = member.Name + " wins!"
		}
	}
	return snapshot
}

func waitingSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Phase = Waiting
	return withoutReadiness(snapshot)
}
