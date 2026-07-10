# Deploy logs2cookies-bot

## Local / VPS

```bash
cd logs2cookies-bot
cp .env.example .env
# fill TELEGRAM_API_ID, TELEGRAM_API_HASH, TELEGRAM_BOT_TOKEN

go mod tidy
go build -ldflags="-s -w" -o logs2cookies .
./logs2cookies
```

Optional:

```bash
export WORK_ROOT=/var/lib/logs2cookies/work
# install 7-Zip for multi-volume fallback
#   Debian/Ubuntu: apt install p7zip-full
#   or drop 7zz on PATH
```

Use a process manager (`systemd`, `tmux`, `screen`) for 24/7.

### systemd sketch

```ini
[Unit]
Description=logs2cookies Telegram bot
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/logs2cookies-bot
EnvironmentFile=/opt/logs2cookies-bot/.env
ExecStart=/opt/logs2cookies-bot/logs2cookies
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

---

## GitHub Actions (manual, up to 6h)

### One-time setup

1. Create a **private** GitHub repo and push this folder.
2. **Settings → Secrets and variables → Actions → New repository secret**
   - `BOT_TOKEN` — from [@BotFather](https://t.me/BotFather)
   - `TELEGRAM_API_ID` — from [my.telegram.org/apps](https://my.telegram.org/apps)
   - `TELEGRAM_API_HASH` — same page

### Start the bot

1. Repo → **Actions**
2. **Run logs2cookies bot** (workflow)
3. **Run workflow**:
   - **no — run checks only** — tests/vet/fmt/build (safe default)
   - **yes — deploy bot after checks** — then starts the bot on Windows
4. Duration: 1 / 2 / 4 / 6 hours when deploying

Pushes to `main` run **CI** only (no deploy).

Deploy runs only after:  
`go mod verify` · tidy · `gofmt` · `go vet` · `go test -race` · `go build`

Temp data: **`D:\botdata\work`** on `windows-latest` (large disk).

### Notes

- Max **6 hours** per GitHub-hosted job
- Only **one** bot run at a time (concurrency cancels previous)
- Disk is ephemeral — presets and work files die with the job
- Multi-part: incomplete sets extract best-effort; Windows runners often have 7-Zip
- For 24/7 use a VPS (Hetzner, Railway, etc.)

### Push

```bash
cd logs2cookies-bot
git init
git add .
git commit -m "logs2cookies-bot"
git branch -M main
git remote add origin https://github.com/YOUR_USER/YOUR_REPO.git
git push -u origin main
```

**Never commit tokens or API credentials** — use GitHub Secrets / `.env` only.
