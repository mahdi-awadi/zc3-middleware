# Executables

Prebuilt, self-contained binaries are published on the
[**Releases**](https://github.com/mahdi-awadi/zc3-middleware/releases) page —
each is a single file with the web console embedded (no runtime, no deps).

| Asset | Run on |
|---|---|
| `GymGate-<ver>-x64.exe` / `GymGate-<ver>-portable.exe` | **Windows desktop app** (reception PC) — Electron installer / portable |
| `zc3-middleware-windows-amd64.exe` | Windows, headless console build (bench / scripting) |
| `zc3-middleware-linux-amd64` | Linux server / VPS (x86-64) |
| `zc3-middleware-linux-arm64` | Linux on ARM (e.g. Raspberry Pi) |
| `checksums.txt` | SHA-256 of each binary |

## Cutting a release

Binaries are built by CI (`.github/workflows/release.yml`) when you push a
version tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```

The workflow runs the tests, builds all three targets, and attaches them to a
new GitHub Release with generated notes.

## Build locally instead

```bash
make build          # host binary -> bin/middleware
make build-linux    # static linux/amd64 -> dist/middleware
make build-arm64    # static linux/arm64 -> dist/middleware-arm64
scripts\build-windows.ps1   # Electron desktop app -> desktop\release\*.exe (Windows host)
```

## Running

```powershell
# Windows desktop app — run the installer (or the portable exe). It starts the
# gate server, shows the console in a window and stays in the tray when the
# window is closed. Data: %APPDATA%\GymGate (gym.json, gym-photos\, server.log);
# an optional .env next to the installed exe is honoured.

# Windows headless build (bench / scripting)
$env:PANEL_ADDR="192.168.0.102:4370"; $env:ALLOW_WRITES="true"
.\zc3-middleware-windows-amd64.exe
```

```bash
# Linux
chmod +x zc3-middleware-linux-amd64
PANEL_ADDR=192.168.0.102:4370 ALLOW_WRITES=true ./zc3-middleware-linux-amd64
```

Then open <http://localhost:8000/>. All settings are environment variables or
a `.env` file — see [`../.env.example`](../.env.example). `PANEL_ADDR` is the
first-boot fallback; gates added from the console (البوابات) take over.
