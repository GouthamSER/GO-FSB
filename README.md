# gofilestream

Go port of TG-FileStreamBot core: `/start` `/help`, receive media → forward to
BIN_CHANNEL → generate a range-request-capable streaming/download link.
Built on `github.com/gotd/td`. **Compiles clean** — verified with `go build`
+ `go vet` in a sandbox against gotd v0.110.0 (go1.22).

## Scope (per user decision — "core only")

Ported: bot start/help, media→link generation, HTTP range streaming.
**Dropped**: MULTI_TOKEN multi-client pool, FSUB, mongo user-db, /stats,
/broadcast. Only single-bot-client, no persistence beyond the session file.

## Known limitations

- **Single client only** — no load-balancing across multiple bot tokens.
- **Same-DC only** — file download uses `upload.getFile` directly against
  whatever DC the client session is on. If BIN_CHANNEL's media lives on a
  different DC (`FILE_MIGRATE_X`), streaming will fail. custom_dl.py's
  cross-DC `generate_media_session` (exported-auth import into a second DC
  session) was intentionally not ported for this pass.
- **CDN redirect not handled** — `upload.fileCdnRedirect` responses error out
  instead of being followed.
- Bot must already be a member/admin of BIN_CHANNEL *and* have exchanged at
  least one message there before `resolveBinChannel` can find its access
  hash (it walks `messages.getDialogs`).
- No HTML formatting/inline buttons in replies — plain text only, kept
  simple on purpose.

## Build

```
go mod tidy   # needs real internet — pulls gotd/td + deps from the module proxy
go build .
```

## Run

Set env vars (subset of the python bot's `.env`):

```
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
```

```
./gofilestream
```

First run needs the bot to receive/see at least one message in BIN_CHANNEL
(e.g. forward anything there manually once) so dialog resolution can find it.
