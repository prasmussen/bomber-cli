# bomber-cli

Multiplayer Bomberman in your terminal, served over SSH with Wish and Bubble Tea.
Two to four players share a room; anyone can join without an account or password.

## Gameplay

https://github.com/user-attachments/assets/c786a28f-1aaa-4017-92f4-2a95f1b222ca

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
New keys are written and synced to a temporary file, then atomically published
without replacing an existing key. Concurrent starts reuse the winning key;
failed writes remove the temporary file so startup can be retried.

```text
--listen        :2323
--host-key      ./data/ssh_host_ed25519_key
--max-sessions  128
--max-rooms     32
--max-sessions-per-ip  16
--idle-timeout  10m
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
movement starts at 100 ms per step and improves by 25 ms to a 25 ms minimum.
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
Initial PTY requests and resizes are bounded to 10,000 columns/rows; malformed
PTY, resize, and shell requests are rejected. Closing a connection cancels its
pending handshake timer.

Public-server defaults also enforce:

- New connections share a server-wide admission budget of 20 per second with
  a burst of 128, checked before SSH key exchange.
- At most 16 connections per source IP, including pending handshakes. This
  allowance is shared by players behind the same NAT; adjust
  `--max-sessions-per-ip` for your audience.
- Disconnect after ten minutes without terminal input (`--idle-timeout`). SSH
  keepalives and server output do not extend this timeout. Connections have an
  absolute four-hour lifetime; players can reconnect afterward.
- Incoming transport traffic is limited to 64 KiB/s with a 256 KiB burst per
  connection. Terminal input is limited to 1 KiB/s with an 8 KiB burst. Exceeding
  either budget closes the connection. Network writes time out after 15 seconds.
- Bracketed paste is disabled, and clients sending its opening marker are
  disconnected. The terminal parser receives short, bounded text chunks to
  prevent unbounded accumulation of continuous printable input.

These are application limits, not protection against a distributed network or
connection flood. Apply connection-rate filtering at the public firewall or
provider edge, and keep administrative SSH on a separate port. The per-IP limit
uses the immediate TCP peer, so a TCP proxy would share that allowance across
its clients. Usernames are display names, not authenticated identities.

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
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --memory 512m --cpus 2 --pids-limit 256 --ulimit nofile=1024:1024 \
  --log-opt max-size=10m --log-opt max-file=3 \
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
in the host firewall. The unit caps memory at 512 MiB (384 MiB soft threshold),
CPU at two cores, tasks at 256, and open files at 1024. The Docker example applies
similar limits and bounds container log retention. Tune these against measured
load before increasing the connection count; memory exhaustion can restart the
service and discard active matches. Keep the state directory writable only by
the service user and configure journal retention on the host.

## Local development scripts

Use `./scripts/dev.sh` for repeatable checks and managed test sessions (Python
3.9+, Go, and OpenSSH required on macOS or Linux):

```sh
./scripts/dev.sh test            # lint, go test -race, go vet, and a test build
./scripts/dev.sh smoke           # real SSH UI/exit check; cleans up its server
./scripts/dev.sh latency         # measure real SSH keypress-to-render latency
./scripts/dev.sh stress          # repeat race-enabled churn and lifecycle tests
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

For a production-capacity load test with Docker running:

The [recorded 128-player test](LOAD_TEST.md) includes measured latency, CPU,
memory, rejection, and reconnection results.

```sh
python3 scripts/loadtest.py
```

This builds the current server and a separate SSH client generator, then starts
an isolated container with two CPUs, 512 MiB RAM (no swap), 256 tasks, and 1024
open files. No host ports are published. The generator shares its network
namespace but has its own CPU/memory allocation, and uses eight loopback source
IPs so the production 16-connections-per-IP policy remains enabled.

The test fills 32 rooms with 128 players, checks rejection of 16 excess clients,
and moves all players every 150 ms for two minutes. It measures actual
keypress-to-changed-arena-output latency on one player per room. It then tests
bomb explosions, results, rematches, complete disconnection, and a second wave
of 128 players. Cgroup CPU/memory/OOM counters and server RSS are sampled about
once per second. The cgroup memory peak also captures allocations between samples;
resource sampling adds a small `docker exec` process to the server cgroup.

Raw events, resource samples, server logs, and container settings are saved under
`.local-test/loadtest/<UTC timestamp>/`. The script removes only its own uniquely
named containers afterward. `--duration 30s` shortens the movement phase for a
smoke run; `--skip-build` reuses the previous load-test image and client binary.
Loopback latency excludes Internet delays and terminal-emulator drawing time.

```sh
golangci-lint run ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
go build -o bin/bomber-cli ./cmd/bomber-cli
```

CI runs the vulnerability scanner on pushes, pull requests, and a weekly
schedule, so new advisories can be detected without a code change. The security
pass found no reachable known vulnerabilities; the module-only OpenPGP advisory
GO-2026-5932 concerns a package this application does not import.

Tests cover deterministic rules, map connectivity/safety, power-up probabilities,
room lifecycle, queue bounds, and slow-client isolation. Integration tests start
real SSH servers on ephemeral loopback ports and connect multiple SSH clients,
exercise ready-up, a match, resizing, disconnects, and denied capabilities.

The stress command runs ten repetitions of the lifecycle tests on ephemeral
loopback ports. Each repetition cycles through 80 SSH sessions, with 16 connected
at once, floods input and resizes, and verifies connection-slot and room cleanup.
It checks goroutine recovery and retained Go heap after garbage collection;
these are regression bounds, not a measurement of peak memory or process RSS.
It also stalls a real SSH transport's writes to verify that another player can
continue playing, and shuts down with that stalled client, an active match, and
a pending handshake. Host-key tests cover concurrent creation and, on macOS and
Linux, a forced partial write in an isolated process. All tests are also included
in the normal test suite; the stress command repeats them to catch timing races.

For a manual check, open the two SSH terminals above, quick join in each, and
press Enter again to ready. From the top-left spawn, move one cell right,
drop a bomb with Space, then move left and down to escape its blast through
the guaranteed clear spawn exits. Verify the second terminal sees the same action. Let a player
die, verify the winner and score, ready for a rematch, resize one terminal below
60×24, restore it, and leave with Escape. Ctrl-C exits.

`internal/game` accepts explicit timestamps and a random seed. `internal/room`
owns each room in one goroutine, processing inputs immediately and advancing
bomb timers and round state at 20 Hz. Due explosions and the match timeout are
also resolved before gameplay inputs, so an input cannot bypass an expired
deadline between ticks. Its input queue has 64 slots, control
queue 16, and each session gets a one-slot latest-snapshot channel. Snapshots
contain only values and fixed arrays. A full frame slot is replaced, so network
writers never block matches. `internal/ui` receives snapshots as they arrive through
Bubble Tea, which renders at up to 60 FPS; `internal/host` owns SSH policy, keys, limits, and shutdown.
