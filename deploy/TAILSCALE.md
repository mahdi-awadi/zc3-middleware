# Connecting the middleware to the panel over Tailscale

The C3 panel is **pull-only** (it never dials out) and its protocol has **no
authentication**. So:

- Something must reach *into* the panel's LAN to talk to port 4370.
- You must **never** port-forward 4370 to the internet.

Tailscale solves both: a **subnet router** on the panel's LAN advertises the LAN
to your tailnet, and the middleware (on your German VPS) reaches the panel over
the encrypted mesh — no router config, no public exposure.

```
[C3 panel 192.168.0.102:4370]
        │  LAN
[ LAN device running Tailscale --advertise-routes ]  ← Raspberry Pi / mini-PC / always-on box
        │  WireGuard mesh (NAT-traversed)
[ VPS (Germany) running zc3-middleware ]  →  PANEL_ADDR=192.168.0.102:4370
        │  HTTPS REST+SSE
[ your frontend ]
```

## 1. Subnet router on the panel's LAN

On an always-on Linux device on the same LAN as the panel (a Raspberry Pi is
ideal). This box must stay on for the panel to be reachable.

```bash
curl -fsSL https://tailscale.com/install.sh | sh

# allow it to forward for the LAN
echo 'net.ipv4.ip_forward = 1' | sudo tee /etc/sysctl.d/99-tailscale.conf
echo 'net.ipv6.conf.all.forwarding = 1' | sudo tee -a /etc/sysctl.d/99-tailscale.conf
sudo sysctl -p /etc/sysctl.d/99-tailscale.conf

# advertise the panel's /24 (adjust to your actual LAN subnet)
sudo tailscale up --advertise-routes=192.168.0.0/24
```

Then in the Tailscale admin console → Machines → this node → **approve the
advertised subnet route**. (Optionally disable key expiry on this node so it
never drops off.)

## 2. VPS (Germany)

```bash
curl -fsSL https://tailscale.com/install.sh | sh
sudo tailscale up --accept-routes     # accept the advertised LAN route
```

Verify reachability from the VPS:

```bash
nc -vz 192.168.0.102 4370      # should connect
```

## 3. Run the middleware on the VPS

```bash
sudo mkdir -p /opt/zc3-middleware
sudo cp middleware /opt/zc3-middleware/
sudo cp .env.example /opt/zc3-middleware/.env    # then edit:
#   PANEL_ADDR=192.168.0.102:4370
#   API_KEY=$(openssl rand -hex 32)
#   HTTP_ADDR=:8000
sudo cp deploy/middleware.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin zc3 2>/dev/null || true
sudo systemctl daemon-reload
sudo systemctl enable --now middleware
curl -H "Authorization: Bearer $API_KEY" localhost:8000/healthz
```

## Notes

- **Latency:** every panel command now crosses Iraq↔Germany over the mesh
  (~tens of ms). Fine for enroll/open/events; the panel still makes access
  decisions locally, so a mesh hiccup never locks anyone out.
- **Alternative (lower latency, recommended for production):** run the
  middleware *on the LAN box itself* and expose only its authenticated REST API
  to the VPS/frontend over the tailnet. Same Tailscale install; skip the subnet
  route and point the frontend at `http://<lan-box-tailscale-ip>:8000`.
- **Firewall:** restrict the middleware's API to the tailnet (`tailscale serve`
  or bind `HTTP_ADDR` to the Tailscale interface) so it isn't exposed on the
  VPS's public interface.
