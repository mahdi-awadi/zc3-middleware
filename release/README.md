# Prebuilt executables

Self-contained binaries — the web console and all assets are embedded, so a
single file is all you need. No runtime, no dependencies.

| File | Run on |
|---|---|
| `zc3-middleware-windows-amd64.exe` | Windows (bench testing / same-LAN PC) |
| `zc3-middleware-linux-amd64` | Linux server / VPS (x86-64) |
| `zc3-middleware-linux-arm64` | Linux on ARM (e.g. Raspberry Pi) |

## Windows (native — no WSL)

Your Windows PC is on the same LAN as the panel, so it can talk to it directly.
In PowerShell, from this folder:

```powershell
$env:PANEL_ADDR="192.168.0.102:4370"   # the panel's LAN IP
$env:ALLOW_WRITES="true"               # enable card enrol/revoke
.\zc3-middleware-windows-amd64.exe
```

Then open <http://localhost:8080/ui>.

## Linux

```bash
chmod +x zc3-middleware-linux-amd64
PANEL_ADDR=192.168.0.102:4370 ALLOW_WRITES=true ./zc3-middleware-linux-amd64
```

Open <http://localhost:8080/ui>.

## Configuration

Every setting is an environment variable (or a `.env` file next to the binary).
See [`../.env.example`](../.env.example). Only `PANEL_ADDR` is required.

Common ones:

| Variable | Purpose |
|---|---|
| `PANEL_ADDR` | `host:port` of the C3 panel (required) |
| `HTTP_ADDR` | API/UI listen address (default `:8080`) |
| `ALLOW_WRITES` | `true` to allow card enrol/revoke and role writes |
| `API_KEY` | bearer token; blank disables auth |
| `ACCESS_CONFIG_PATH` | roles/policy file (default `access.json`) |

> These binaries are built from commit `0a51f77`. Rebuild anytime with
> `make build-linux` / `make build-arm64`, or the `go build` lines in the
> project Makefile.
