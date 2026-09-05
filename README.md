# bomber-cli

Multiplayer Bomberman in your terminal, served over SSH with Wish and Bubble Tea.
Two to four players share a room; anyone can join without an account or password.

## Build and run

Requires Go **1.27.1**. Dependencies are pinned in `go.mod` and `go.sum`.

```sh
go build -o bin/bomber-cli ./cmd/bomber-cli
./bin/bomber-cli
ssh -p 2323 alice@localhost
# In a second terminal:
ssh -p 2323 bob@localhost
```

The first connection prompts you to trust the SSH host key. The server generates
an Ed25519 private key with mode 0600 and reuses it on subsequent starts. Persist
this file across upgrades so clients keep recognizing the server.

```text
--listen        :2323
--host-key      ./data/ssh_host_ed25519_key
--max-sessions  128
--max-rooms     32
```

Use a terminal of at least **60 columns by 24 rows**. The game uses ASCII symbols
and ANSI colors, with no emoji or special fonts. `TERM=dumb` disables colors.
A smaller window displays a resize prompt and ignores gameplay keys while the
match continues; Escape and Ctrl-C still work.

## Play

- Lobby: Up/Down or W/S selects quick join, create room, or a listed room. Enter joins.
- Ready-up: Enter toggles ready. At least two players and everyone ready starts a
  three-second countdown. Joining, leaving, or unreadying cancels it. Membership
  changes reset everyone's ready state.
- Match: Arrow keys or WASD move; Space drops a bomb. Key repeat works, subject to
  the server's movement cooldown. Escape leaves the room; Ctrl-C disconnects.
- Eliminated players watch the surviving players. Results display for three seconds,
  then the room returns to ready-up. Wins persist while you remain in that room.
  Leaving discards your room score. Active and result rooms reject joins.

The arena is 15×13 cells, each two characters wide. `##` is a solid wall, `[]` a
destructible block, `P1`–`P4` players, pulsing `o*`/`O*` bombs, and `**` fire.
Bombs pulse at a steady rhythm without revealing their remaining fuse time.
A player standing on a bomb is drawn above it. `B+`, `R+`, and `S+` increase bomb
capacity, blast range, and speed. Player numbers correspond to corner spawns.

Bombs detonate after two seconds, triggering other bombs. Flames last 500 ms and
kill every player, including the bomb owner. You can step off your own bomb but
cannot walk back through it. Blasts stop at walls and the first block they hit.
Destroyed blocks have a 30% chance of dropping one of the three upgrades equally.
Capacity starts at one and caps at five; range starts at two and caps at eight;
movement starts at 150 ms per step and improves by 25 ms to a 75 ms minimum.
Upgrades reset each round. Last survivor wins; simultaneous final deaths or a
three-minute timeout with multiple survivors produce a draw. Leaving eliminates
you immediately; your bombs remain in an ongoing match.

## Hosting

Expose the configured **TCP** port through your firewall/router. No HTTP proxy is
needed. This is a game-only SSH endpoint: interactive PTY shells are required;
commands, SCP, SFTP, agent/X11 forwarding, and TCP forwarding are rejected.
Each connection is limited to one session channel. Connections that do not open
an interactive shell within ten seconds are closed, including stalled handshakes.
The connection limit includes those pending connections.

Usernames become bounded ASCII display names; duplicates get numeric suffixes.
Internal player IDs are independent of those names. Rooms and scores live only
in memory. Empty rooms are removed. There are no accounts, database, chat, bots,
or reconnection recovery. Connection and match events go to stderr. SIGINT and
SIGTERM close connections, stop room goroutines, and shut down the listener.

### Docker

```sh
docker build -t bomber-cli .
docker volume create bomber-data
docker run --rm --name bomber-cli -p 2323:2323 \
  -v bomber-data:/data bomber-cli
```

The image runs as UID/GID 10001. Named volumes preserve the generated host key.
If using a bind mount, make it writable by UID 10001 before starting. Pass CLI
flags after the image name to override defaults, retaining `--host-key /data/ssh_host_ed25519_key`.

### Linux systemd

```sh
sudo install -m 0755 bin/bomber-cli /usr/local/bin/bomber-cli
sudo install -m 0644 deploy/bomber-cli.service /etc/systemd/system/bomber-cli.service
sudo systemctl daemon-reload
sudo systemctl enable --now bomber-cli
journalctl -u bomber-cli -f
```

Build the binary on Linux, or cross-compile with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/bomber-cli ./cmd/bomber-cli`
(use `arm64` for an ARM host). The unit uses a dynamic unprivileged user and a
persistent `/var/lib/bomber-cli` state directory for the host key. Open TCP 2323
in the host firewall. Public deployment is outside this repository's scope.

## Local development scripts

Use `./scripts/dev.sh` for repeatable checks and managed test sessions (Python
3.9+, Go, and OpenSSH required on macOS or Linux):

```sh
./scripts/dev.sh test            # go test -race, go vet, and a test build
./scripts/dev.sh smoke           # real SSH UI/exit check; cleans up its server
./scripts/dev.sh start           # build and start on 127.0.0.1:23230
./scripts/dev.sh ssh alice       # first terminal
./scripts/dev.sh ssh bob         # second terminal
./scripts/dev.sh status
./scripts/dev.sh kill-sessions   # disconnect managed clients; keep server
./scripts/dev.sh stop            # stop managed clients and test server
```

`build` is also available separately. The test executable, host key, known-hosts
file, server log, and process records live in ignored `.local-test/`. Normal
hosting on port 2323 and its `data/` directory are unaffected. The script refuses
to take over an occupied test port. Cleanup checks each recorded PID's launch
time and full command before signaling it; stale records never trigger a kill.
Untracked SSH clients are disconnected when their managed server is stopped.

## Verification and architecture

```sh
go test -race ./...
go vet ./...
go build -o bin/bomber-cli ./cmd/bomber-cli
```

Tests cover deterministic rules, map connectivity/safety, power-up probabilities,
room lifecycle, queue bounds, and slow-client isolation. Integration tests start
real SSH servers on ephemeral loopback ports and connect multiple SSH clients,
exercise ready-up, a match, resizing, disconnects, and denied capabilities.

For a manual check, open the two SSH terminals above, quick join in each, and
press Enter again to ready. From the top-left spawn, move one cell right,
drop a bomb with Space, then move left and down to escape its blast through
the guaranteed clear spawn exits. Verify the second terminal sees the same action. Let a player
die, verify the winner and score, ready for a rematch, resize one terminal below
60×24, restore it, and leave with Escape. Ctrl-C exits.

`internal/game` accepts explicit timestamps and a random seed. `internal/room`
owns each room in one goroutine at 20 Hz; its input queue has 64 slots, control
queue 16, and each session gets a one-slot latest-snapshot channel. Snapshots
contain only values and fixed arrays. A full frame slot is replaced, so network
writers never block matches. `internal/ui` consumes these snapshots through
Bubble Tea; `internal/host` owns SSH policy, keys, limits, and shutdown.
