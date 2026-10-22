# block52 chat

A small, **in-memory** WebSocket chat server. Messages are **Cosmos-signed** and
verified server-side. Chat rooms are **per table** — anyone can post to a table
(access control is deliberately left open for now, to be tightened later).

Nothing is persisted. Messages live in a bounded per-table ring buffer and are
purged by TTL; whole table groups are reaped once idle. A restart loses all chat.

## How it works

- **Transport:** one WebSocket per client at `GET /ws?table=<name>`. A client
  joins exactly one table and receives a short backlog of recent messages on
  connect, then live broadcasts.
- **Auth (per message):** a client sends `{ address, pubKey, signature, message, text }`.
  The server:
  1. derives the bech32 address from `pubKey` (`ripemd160(sha256(pubkey))`) and
     checks it equals `address`;
  2. verifies `signature` over the ADR-36 "sign arbitrary data" doc for `message`;
  3. requires `message == text` so a signature can't be lifted onto other content.
  This matches Keplr's `signArbitrary` / cosmjs `verifyADR36Amino`. See
  `internal/auth/cosmos.go` and its tests for the exact signing scheme (including a
  non-browser signing reference).
- **Rooms:** `internal/hub` keeps a map of table → (ring buffer + subscribers).
  Slow subscribers are skipped, never block the room.
- **Retention:** a background reaper purges expired messages and idle tables.

## Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `CHAT_LISTEN` | `:8080` | Listen address |
| `CHAT_EXPECTED_HRP` | *(any)* | Require this bech32 prefix, e.g. `cosmos` |
| `CHAT_ALLOWED_ORIGINS` | *(same-origin)* | CSV of allowed browser origins for the WS Origin check |
| `CHAT_MAX_MESSAGES` | `200` | Ring-buffer size per table |
| `CHAT_MESSAGE_TTL_SEC` | `3600` | Drop messages older than this |
| `CHAT_TABLE_IDLE_TTL_SEC` | `7200` | Reap a table after this long idle |
| `CHAT_SWEEP_SEC` | `300` | Reaper interval |
| `CHAT_MAX_MSG_BYTES` | `4096` | Max inbound frame size |

## Run locally

```bash
go run ./cmd/server          # then open http://localhost:8080
go test ./...
```

## Docker

The image is distroless (no shell, non-root) and mounts **no volumes** — state is
intentionally ephemeral. The container binds to loopback; **nginx on the VM** is
the public entrypoint and terminates TLS.

```bash
docker compose up --build -d
```

Health: `GET /healthz` → `{"status":"ok","tables":N,"subscribers":M}`. The
compose healthcheck runs `chat -healthcheck` (the binary probes `/healthz`
itself, since distroless has no curl).

## nginx (on the VM)

See `deploy/nginx.conf`: TLS termination plus the WebSocket `Upgrade` headers and
a long `proxy_read_timeout` so idle chat sockets survive. Copy it into
`sites-available`, point `server_name`/cert paths at your host, `nginx -t`,
reload.

## Client

`web/index.html` is a reference browser client using Keplr. For bots/servers,
sign the same ADR-36 StdSignDoc and send the same JSON envelope.

## Not done yet (by design)

- No per-table authorization (anyone can post). Add an allowlist / table-owner
  check when ready.
- No rate limiting or spam control.
- No persistence (intentional).
