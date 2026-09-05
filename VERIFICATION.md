# Local verification

Verified on 2026-09-05 using Go 1.27.1 on macOS/arm64 and Docker Desktop's
Linux/arm64 engine. No public deployment was performed.

## Automated gates

- `go test -race ./...` — passed, including real loopback SSH integration tests.
- `go vet ./...` — passed.
- `go build -o bin/bomber-cli ./cmd/bomber-cli` — passed.
- `docker build -t bomber-cli:local .` — passed.

## Plan coverage

| Requirement | Implementation and verification |
| --- | --- |
| 15×13 symmetric maps, walls, pillars, blocks, safe spawns, connected routes | `internal/game`; `TestMapSafetySymmetryAndConnectivity`, 100 seeds |
| Movement, collision, 100 ms cooldown, leave-own-bomb behavior | `TestMovementAndBombs` |
| Two-second fuse, 500 ms flames, four-direction blast blocking | `TestBlastBlockingAndFlameLifetime` |
| Capacity limits and replenishment | `TestCapacityCountsOnlyOwnedLiveBombs` |
| Chain reactions and simultaneous final deaths | `TestChainAndSimultaneousDeaths` |
| Instant flame/owner deaths | `TestFlamesKillOnEntryAndOwner` |
| Three upgrades, probability, equal weighting, caps, round defaults | `TestPowerUpsAndCaps`, `TestSeededRoundsAndDrops` (10,000 drops) |
| Leaving, retained bombs in ongoing rounds, winner and time-limit draw | `TestEliminationAndTimeLimit` |
| Deterministic rules with explicit time and seed | `game.New`, `Move`, `Place`, `Tick`; `TestDeterministicRoundReplay` |
| Four-player capacity, everyone-ready gate, three-second countdown, membership cancellation, active join rejection | `TestReadyMembershipCountdownAndCapacity` |
| Disconnect result, score retention, reset upgrades, rematch | `TestRematchScoresAndDisconnect` |
| Solo waiting and scoreless draw | `TestSoloNeverStartsAndDrawHasNoScore` |
| Quick join, create, limits, unique sanitized names/IDs, empty-room cleanup | `TestHubLimitsCleanupNamesAndQuickJoin`; UI lobby controls |
| One actor per room with immediate input processing and 20 Hz timers; bounded inputs and replaceable value-only snapshots | `Room.run`, `Submit`, `publish`; `TestSlowClientAndBoundedInput` under race detector |
| Full-screen color/ASCII UI, two-column tiles, numbers, pulsing bombs, fire, upgrades, score and controls | `Model.View`; render-size test and manual SSH match |
| Room selection and scrolling at minimum size | `TestLobbyScrolling` |
| Minimum 60×24, resize prompt, gameplay suppression, Escape/Ctrl-C | UI tests and `TestRealSSHMultiplayerResizeAndDisconnect` |
| Spectating after death | Snapshot retains eliminated members; game rejects dead-player moves/bombs; UI shows `out` and continues the shared board |
| Real multi-session SSH play and duplicate display names | `TestRealSSHMultiplayerResizeAndDisconnect`; manual match below |
| Interactive PTY only; no exec/SCP/SFTP/agent/X11/TCP forwarding | `TestDeniedCapabilities` |
| Persistent restrictive host key, pending-connection limit, handshake timeout | `TestHostKeyPersistencePermissionsAndLimits`; Docker restart check |
| Bounded, nonblocking resizes, including before shell startup | `TestResizeBeforeShellAndFlood` |
| Channel-only disconnect cleanup | `TestChannelCloseDisconnectsPlayer` |
| Defaults and invalid configuration | `host.DefaultConfig`, CLI flags, `TestInvalidConfigAndHostKey` |
| Event logging and graceful termination | Manual server logs, integration cleanup, SIGTERM and Docker stop exit 0 |
| Runnable executable, Docker image, Linux systemd example, port/key documentation | `bin/bomber-cli`, `Dockerfile`, `deploy/bomber-cli.service`, `README.md` |
| In-memory scope; no accounts, persistence of scores, bots, chat or recovery | Separate game/room/UI/host packages; only host key touches persistent storage |

## Manual two-terminal match

Opened two independent OpenSSH PTYs at 60×24 against the local executable on
127.0.0.1:23230, as `alice` and `bob`. Quick joined the same room and readied both.
Observed the three-second countdown and the same arena in both terminals. Moved
Alice right and back left, placed a bomb, and moved Bob left. Both terminals
showed the explosion. Alice died; both
terminals displayed `bob wins!`, Bob's score became 1, and the room returned to
ready-up. Readied both again and observed a fresh map and retained score.
Escape returned both to the lobby; Ctrl-C restored both terminals and exited SSH.
The temporary server exited successfully on SIGTERM.

## Container checks

Built and ran the Linux image on loopback port 23231. Confirmed UID/GID 10001,
key mode 0600, and identical key hash after restart and after replacing the
container while retaining its volume. Connected using a real OpenSSH PTY and
observed the lobby. `docker stop` completed with exit code 0. Temporary test
containers and volumes were removed; the `bomber-cli:local` image remains.

The systemd unit is supplied as a Linux hosting example. It was inspected but
not installed into a running systemd host in this macOS environment.
