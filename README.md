# 🚀 gofilestream

> Go port of TG-FileStreamBot's core — send a file, get an instant streaming/download link.

**👤 Owner:** [GouthamSER](https://github.com/GouthamSER)

Built on [`gotd/td`](https://github.com/gotd/td) (raw MTProto in Go, no CGo).
✅ Verified with `go build` + `go vet` in a clean-room test against gotd v0.110.0 (go1.22).

---

## ✨ What it does

- 🤖 `/start`, `/help`, `/stats` bot commands
- 📤 Send any document / video / audio / photo → bot forwards it to your storage channel and replies with a link + a tappable ⬇️ Download button
- 🎬 That link supports HTTP Range requests, so it streams/seeks straight in a browser or video player — no full download needed
- 📥 Every upload also posts a note in `BIN_CHANNEL` — sender name, username, user ID, file name — so admins can see who sent what
- 📊 `/stats` reports live CPU / RAM / storage / uptime (reads `/proc` + `statfs` directly, Linux only — fine for the Docker deploy, won't build on macOS/Windows)
- 📢 Optional channel button on `/start` if `CHANNEL_URL` is set
- 🔒 Optional force-subscribe gate — set `FSUB_CHANNEL` and both `/start` and file uploads check membership first, with a "Join Channel" button if not subscribed

## 🧩 Scope

This is a **"core only"** port — a deliberate scope cut, not an oversight.

| Ported ✅ | Dropped ❌ |
|---|---|
| bot `/start` `/help` `/stats` | MULTI_TOKEN multi-client pool |
| media → link generation | MongoDB user-db |
| HTTP range streaming | `/stats`'s user-count / `/broadcast` (no user-db) |
| force-sub gate | |

---

## ⚠️ Known limitations

- **🔑 Bin-channel discovery is passive.** Bots can't call `messages.getDialogs`
  or `messages.checkChatInvite` (`BOT_METHOD_INVALID` on both). Instead, the
  bot learns `BIN_CHANNEL`'s access hash the moment a live update mentions
  it. **First run:** open the channel and remove+re-add the bot as admin (or
  post any message there) to trigger it.
  - 💾 Once learned, it's cached to disk next to the session file — same-disk restarts skip discovery.
  - ☁️ On ephemeral-disk platforms (Koyeb etc.) that cache doesn't survive a redeploy — instead, copy the `BIN_CHANNEL_ACCESS_HASH=...` value the logs print after first resolution into an env var, and every future deploy resolves instantly.
  - `/start` and `/help` work immediately either way — only file-link generation waits on this.
  - **`FSUB_CHANNEL` uses this exact same bootstrap** (own cache file, own `FSUB_CHANNEL_ACCESS_HASH` override) — if set, do the re-add-admin step for that channel too. Until it resolves, the fsub gate fails **closed** (blocks with a "try again" message) rather than open.
- **👤 Single bot client only** — no multi-token load balancing.
- **🌍 Same-DC only** — `upload.getFile` is called against whatever DC the session is on; if the file lives on a different DC (`FILE_MIGRATE_X`), streaming fails. Cross-DC media sessions weren't ported.
- **📡 No CDN redirect support** — `upload.fileCdnRedirect` responses aren't followed.
- **🚦 Flood protection built in** — concurrent chunk requests are capped app-wide (`MAX_CONCURRENT_DOWNLOADS`, default 32), with automatic retry+backoff on `FLOOD_WAIT` (video players fire lots of overlapping Range requests while seeking, which trips Telegram's rate limit fast without this).
- **⚡ Pipelined downloads** — each stream prefetches up to `PER_STREAM_PARALLEL` chunks ahead (default 24) instead of waiting for one RPC round-trip before starting the next. Bytes are still written to the client strictly in order. Both knobs are env-configurable if your host/DC combo can push more (or needs less to avoid `FLOOD_WAIT`).
- **📈 Throughput is logged per stream** (`stream done for message N: X MB in Yms (Z Mbps)`) — useful for actually diagnosing slow downloads instead of guessing. If speed is still lower than expected after tuning the two knobs above, the remaining bottleneck is most likely single-TCP-connection overhead to Telegram's DC (this Go port, like gotd's own downloader, multiplexes all chunk requests over one connection rather than opening several physical connections the way some Python setups effectively do) or the host's own network path — neither of which more app-level concurrency will fix.
- **⏱️ No artificial stream timeout** — only the initial metadata lookup gets a 30s timeout; the actual byte streaming uses the request's own context (cancels on client disconnect only). An earlier version accidentally reused that 30s timeout for the whole download, silently killing any file that took longer than 30s to finish and causing an endless client-retry/FLOOD_WAIT storm — fixed.
- **✍️ Plain text replies only** — no HTML formatting or inline buttons, kept simple on purpose.

---

## 🛠️ Build

```bash
go mod tidy   # needs real internet — pulls gotd/td + deps from the module proxy
go build .
```

## ▶️ Run

Set env vars (subset of the Python bot's `.env`):

```env
API_ID=...
API_HASH=...
BOT_TOKEN=...
BIN_CHANNEL=-1001234567890
PORT=8080
WEB_SERVER_BIND_ADDRESS=0.0.0.0
HASH_LENGTH=6
FQDN=your.domain.com
HAS_SSL=true
NO_PORT=true
SESSION_FILE=gofilestream.session.json
# BIN_CHANNEL_ACCESS_HASH=...   # optional — skips discovery, see Known limitations
# CHANNEL_URL=https://t.me/yourchannel   # optional — shows a button on /start
# FSUB_CHANNEL=-1009876543210   # optional — force-subscribe gate
# FSUB_CHANNEL_ACCESS_HASH=...   # optional — skips fsub discovery, see Known limitations
# FSUB_CHANNEL_URL=https://t.me/yourchannel   # shown as the "Join Channel" button
# PER_STREAM_PARALLEL=24        # chunks one stream prefetches at once
# MAX_CONCURRENT_DOWNLOADS=32   # global cap across all streams
```

```bash
./gofilestream
```

**🔗 First run:** once it's up, open `BIN_CHANNEL` in Telegram and remove+re-add
the bot as admin (or send any message there) — that's what lets it learn the
channel's access hash. See ⚠️ Known limitations for the full story.

---

## 🐳 Deploy

[![Deploy](https://www.herokucdn.com/deploy/button.svg)](https://heroku.com/deploy?template=https://github.com/GouthamSER/GO-FSB)

`Dockerfile` and `Procfile` included — ready for Koyeb, Render, Railway, or
any Docker-based host. Multi-stage build → static binary on Alpine, with a
health check baked in.

**Heroku** uses `heroku.yml` (container stack) + `app.json`, reusing the same
`Dockerfile` — no separate Heroku-specific build path to maintain.

```bash
heroku create your-app-name --stack container
heroku config:set API_ID=... API_HASH=... BOT_TOKEN=... BIN_CHANNEL=... \
  FQDN=your-app-name.herokuapp.com HAS_SSL=true NO_PORT=true
git push heroku main
```

Or click the button above — it walks through the same env vars via `app.json`.

⚠️ Heroku dynos cycle/restart regularly and have an ephemeral filesystem, so
the same advice from "Bin-channel discovery is passive" above applies here
too: **set `BIN_CHANNEL_ACCESS_HASH`** after the first successful resolution
so every dyno restart doesn't need the re-add-admin dance again.

---

<p align="center">Made for <a href="https://github.com/GouthamSER">@GouthamSER</a> 🐐</p>
