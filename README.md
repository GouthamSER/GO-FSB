# 🚀 gofilestream

> Go port of TG-FileStreamBot's core — send a file, get an instant streaming/download link.

**👤 Owner:** [GouthamSER](https://github.com/GouthamSER)

Built on [`gotd/td`](https://github.com/gotd/td) (raw MTProto in Go, no CGo).
✅ Verified with `go build` + `go vet` in a clean-room test against gotd v0.110.0 (go1.22).

---

## ✨ What it does

- 🤖 `/start` and `/help` bot commands
- 📤 Send any document / video / audio / photo → bot forwards it to your storage channel and replies with a link
- 🎬 That link supports HTTP Range requests, so it streams/seeks straight in a browser or video player — no full download needed

## 🧩 Scope

This is a **"core only"** port — a deliberate scope cut, not an oversight.

| Ported ✅ | Dropped ❌ |
|---|---|
| bot `/start` `/help` | MULTI_TOKEN multi-client pool |
| media → link generation | FSUB gate |
| HTTP range streaming | MongoDB user-db |
| | `/stats`, `/broadcast` |

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
- **👤 Single bot client only** — no multi-token load balancing.
- **🌍 Same-DC only** — `upload.getFile` is called against whatever DC the session is on; if the file lives on a different DC (`FILE_MIGRATE_X`), streaming fails. Cross-DC media sessions weren't ported.
- **📡 No CDN redirect support** — `upload.fileCdnRedirect` responses aren't followed.
- **🚦 Flood protection built in** — concurrent chunk requests are capped at 6 in-flight, with automatic retry+backoff on `FLOOD_WAIT` (video players fire lots of overlapping Range requests while seeking, which trips Telegram's rate limit fast without this).
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
```

```bash
./gofilestream
```

**🔗 First run:** once it's up, open `BIN_CHANNEL` in Telegram and remove+re-add
the bot as admin (or send any message there) — that's what lets it learn the
channel's access hash. See ⚠️ Known limitations for the full story.

---

## 🐳 Deploy

`Dockerfile` and `Procfile` included — ready for Koyeb, Render, Railway, or
any Docker-based host. Multi-stage build → static binary on Alpine, with a
health check baked in.

---

<p align="center">Made for <a href="https://github.com/GouthamSER">@GouthamSER</a> 🐐</p>
