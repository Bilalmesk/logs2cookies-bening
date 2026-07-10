# logs2cookies-bot

Telegram bot written in **Go (MTProto / gogram)** that turns stealer-log archives into clean Netscape cookie dumps.

Send a `.zip` / `.rar` / `.7z` (or a direct URL) → pick domains or a **builtin pack** → get per-victim cookie files back as a zip.

Also ships an **offline CLI** (`extract`) that needs no Telegram credentials.

---

## Features

| Area | Details |
|---|---|
| **Ingest** | Telegram uploads up to **~2 GB** (MTProto), URL downloads up to **5 GB** (parallel range GET) |
| **Formats** | Netscape `.txt`, JSON cookie dumps, **Chrome/Edge SQLite** `Cookies`, **Firefox** `moz_cookies` |
| **Archives** | Zip + RAR (+ 7z via `7zz`/`7z` fallback), nested archives (depth 4), encrypted archives |
| **Multi-part** | Any part accepted (order ignored): `.partN.rar`, `.r00`, `.partN.zip`, `.z01`, `.001`, `.7z.001`… → `/done` |
| **Incomplete sets** | Best-effort extract when parts are missing; optional **7-Zip** fallback for mid-volume recovery |
| **Captions** | `filter:netflix` = domain filter; `Password @HUNTER_CLOUDS` = password **candidate** only (never forced as filter) |
| **Passwords** | Try open without password → if encrypted, try caption password once → then **ask user** (session kept alive) |
| **Quality** | Drops expired + empty/`deleted` values by default; normalizes Chrome WebKit timestamps to Unix |
| **Matching** | Subdomain-aware for `filter:cursor.com`; loose substring for bare tokens like `steam` |
| **Builtin packs** | One-tap: cursor · github · discord · steam · netflix · google · epic · riot · paypal · spotify |
| **User presets** | `/preset save gaming steam epicgames` — in-memory for process lifetime |
| **Packing** | Extract-all / top-50 → **one combined zip**; ≤8 targets → per-domain zips |
| **UI** | HTML progress cards (`<pre>` bars) — long pack names with `_` / `!` / `@` no longer break Telegram layout |
| **Ops** | Disk spool (not RAM), extract queue (2), heartbeats, session janitor (30 min), `.env` loading |

---

## Requirements

- **Go 1.25+** (toolchain auto-download ok with `GOTOOLCHAIN=auto`)
- Telegram app credentials: [my.telegram.org/apps](https://my.telegram.org/apps)
- Bot token: [@BotFather](https://t.me/BotFather)
- Optional: `7zz` / `7z` / `7za` on `PATH` for stronger multi-volume recovery

### Dependencies (via `go.mod`)

- `github.com/amarnathcjd/gogram` — MTProto bot client
- `github.com/nwaples/rardecode/v2` — RAR
- `github.com/yeka/zip` — passworded ZIP
- `modernc.org/sqlite` — Chrome/Firefox cookie DBs (pure Go)

Local `replace` for `golang.org/x/crypto` → `./third_party/crypto` is included so builds work in restricted networks. On normal hosts you can remove the replace line if preferred.

---

## Quick start (Telegram bot)

```bash
cd logs2cookies-bot
cp .env.example .env
# edit .env:
#   TELEGRAM_API_ID=...
#   TELEGRAM_API_HASH=...
#   TELEGRAM_BOT_TOKEN=...

go mod tidy
go run .
# or:
go build -ldflags="-s -w" -o logs2cookies .
./logs2cookies
```

Env vars can also be exported instead of `.env`. Aliases: `APP_ID`, `APP_HASH`, `BOT_TOKEN`.

See **[DEPLOY.md](./DEPLOY.md)** for GitHub Actions hosting (Windows runner, up to 6h).

---

## Telegram usage

### Basic

1. DM the bot → `/start`
2. Send a `.zip` / `.rar` / `.7z` as a **document**
3. After extract, choose:
   - **extract all** / **top 50** → one combined zip
   - a **builtin pack** (shows live hit counts)
   - **browse domains** → paginated multi-select
   - type domains freehand: `netflix paypal steam`
4. Receive per-victim Netscape `.txt` files inside a zip

### Captions

| Caption | Effect |
|---|---|
| `filter:netflix` | Pre-filter domains during extract |
| `filter:cursor.com` | Subdomain-aware domain filter |
| `steam` | Short domain-like token only (not free prose) |
| `✅ Password @HUNTER_CLOUDS` | **Not** a filter — stored as password *candidate* |
| `password: secret` / `pass: x` / `pw=x` | Same — password candidate |

Password captions are applied **only after** the archive reports encryption. If the candidate fails, the bot asks you and **keeps the session** so you can retry.

### Multi-part archives

1. Send **any** part first (part2 alone is fine)
2. Send remaining parts (any order, any common naming)
3. `/done` when ready — incomplete sets still extract best-effort
4. `/cancel` aborts and cleans temp files

Accepted names include:

- `logs.part1.rar` … `logs.part99.rar`, `logs.r00`, `logs.rar.002`
- `logs.part2.zip`, `logs.z01`, `logs.zip.001`
- `logs.001`, `dump.7z.001`
- While a multi-part session is open: **any document** is accepted as another part

### Commands

| Command | Action |
|---|---|
| `/start` `/help` | Help text |
| `/done` | Finish multi-part upload and extract |
| `/cancel` | Abort session, delete job temp files |
| `/preset` | List saved presets |
| `/preset save NAME d1 d2…` | Save domain preset |
| `/preset delete NAME` | Delete preset |

---

## Progress UI

Status updates use **HTML** (not Markdown) so pack filenames with `_` do not italicize/break the bar:

```
[1/3] ⬇️ downloading
📦 @UP_DAISY…5913_ON_CHANNEL.rar
████████░░░░░░  45.9%
1.77GB     / 3.85GB
12.50MB/s  · ETA 2m50s
```

Steps: **`[1/3]` download** → **`[2/3]` extract** → **`[3/3]` pack**.

---

## Offline CLI

No Telegram env required.

```bash
# list builtin packs
./logs2cookies presets
# or: ./logs2cookies extract --list-presets

# extract with a pack
./logs2cookies extract ./logs.zip -p cursor -o ./hits

# domains + cookie names
./logs2cookies extract ./logs.rar -d github.com -n user_session,logged_in

# multi-pack, keep expired/empty if you want
./logs2cookies extract ./logs.zip -p github,discord --keep-expired --keep-empty

# one zip per domain
./logs2cookies extract ./logs.zip -d netflix.com,steamcommunity.com --split

# parallel URL download helper
./logs2cookies download https://example.com/file.zip 20 0
```

### CLI flags

| Flag | Description |
|---|---|
| `-p, --preset` | Builtin pack(s), comma-separated |
| `-d, --domains` | Domain filter (subdomain-aware) |
| `-n, --names` | Cookie name allowlist |
| `-o, --out` | Output directory (default `./out`) |
| `-pw, --password` | Archive password |
| `--keep-expired` | Keep expired cookies |
| `--keep-empty` | Keep empty/deleted values |
| `--split` | One zip per domain |
| `--list-presets` | Print packs and exit |

CLI writes: `cookies.zip`, `cookies.txt`, `cookies.json`, `manifest.json`, `summary.txt`.

---

## Builtin packs

| Pack | Domains (sample) | Cookie names (sample) |
|---|---|---|
| `cursor` | cursor.com, cursor.sh | WorkosCursorSessionToken, session |
| `github` | github.com | user_session, logged_in, _gh_sess |
| `discord` | discord.com, discordapp.com | __dcfduid, __sdcfduid |
| `steam` | steamcommunity.com, steampowered.com | steamLoginSecure, sessionid |
| `netflix` | netflix.com | NetflixId, SecureNetflixId |
| `google` | google.com, youtube.com | SID, HSID, __Secure-1PSID… |
| `epic` | epicgames.com | EPIC_SSO… |
| `riot` | riotgames.com | tdid, ssid… |
| `paypal` | paypal.com | cookie_check… |
| `spotify` | spotify.com | sp_dc, sp_key |

---

## Environment

| Variable | Required | Notes |
|---|---|---|
| `TELEGRAM_API_ID` / `APP_ID` | bot | Numeric app id |
| `TELEGRAM_API_HASH` / `APP_HASH` | bot | App hash |
| `TELEGRAM_BOT_TOKEN` / `BOT_TOKEN` | bot | BotFather token |
| `WORK_ROOT` | no | Temp/job root (default `work/`) |

`.env` is loaded automatically from the working directory (does not override existing env). **Never commit `.env`.**

---

## Limits

| Limit | Value |
|---|---|
| Telegram upload (MTProto) | ~2 GB |
| URL download | 5 GB |
| Single inner file | 50 MB |
| Nested archive depth | 4 |
| Concurrent heavy extracts | 2 (queue + heartbeat) |
| Session TTL | 30 minutes |
| Work dir janitor | 30 minutes |

---

## Project layout

```
logs2cookies-bot/
├── main.go                 # entry, Netscape/JSON parsers, caption meta
├── session.go              # job state machine, packing, handlers
├── archive.go              # zip/rar walk, nest, soft password errors
├── spool.go                # disk-backed cookie stream + zip filter
├── sqlite_cookies.go       # Chrome + Firefox DB parsers
├── quality.go              # expiry / empty / subdomain match
├── builtin_presets.go      # one-tap packs
├── presets.go              # per-user saved presets
├── multipart.go            # zip multi-part join, 7z fallback, upload accept
├── rar_volume.go           # RAR multi-volume resolve (any part index)
├── status.go               # HTML progress cards (download/extract/pack)
├── ui.go                   # friendly errors, progress dots
├── botapi.go               # gogram wrappers, .env load
├── download.go             # parallel URL fetch
├── cli_extract.go          # offline extract command
├── *_test.go               # unit tests
├── third_party/crypto/     # vendored golang.org/x/crypto (go.mod replace)
├── .env.example
├── .github/workflows/      # CI + optional bot runner
├── DEPLOY.md
└── README.md
```

---

## Tests & build

```bash
go test -count=1 ./...
go test -race ./...
go vet ./...
gofmt -l .
go build -ldflags="-s -w" -o logs2cookies .
```

CI (see `.github/workflows/ci.yml`) runs: `go mod verify` · tidy · `gofmt` · `go vet` · `go test -race` · `go build`.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| Progress bar “overlaps” / broken italics | Fixed: status uses HTML; restart bot on latest build |
| `filter=✅ Password @…` zero cookies | Fixed: password captions are not domain filters |
| `wrong password` then can't reply | Fixed: session stays in await-password; reply with real key |
| Only part2 fails | Fixed: any part accepted; `/done` best-effort |
| Encrypted mid-volume RAR | Install `7zz`/`7z` on PATH |
| `missing env: TELEGRAM_API_HASH` | Set in `.env` or export before run |

---

## Security notes

- Treat bot tokens and API hashes as secrets (`.env` gitignored).
- Job directories under `WORK_ROOT` hold raw stealer data — disk is ephemeral on GHA; wipe on VPS as needed.
- Bot processes user-supplied archives; run only for trusted operators.

---

## License / use

Operator tooling for processing archives you are authorized to handle. You are responsible for compliance with applicable law and platform terms.
