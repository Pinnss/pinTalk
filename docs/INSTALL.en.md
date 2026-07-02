# pintalk — Installation Guide

*Читайте эту инструкцию на русском: [INSTALL.ru.md](INSTALL.ru.md).*

pintalk is a single static Go binary: web UI, signaling, TURN relay and automatic
Let's Encrypt certificates are all built in. No Docker, no database, no external
dependencies.

This guide covers two targets:

- [Linux (Ubuntu / Debian, systemd)](#linux-ubuntu--debian)
- [OpenWrt (routers, e.g. BananaPi R3)](#openwrt)

## Prerequisites (both platforms)

1. **A domain pointing at your public IP** — e.g. `call.example.com` must resolve
   to the machine's WAN address. Let's Encrypt won't issue a certificate otherwise.
2. **Ports reachable from the internet**:
   - `80/tcp` — ACME HTTP-01 challenge + redirect to HTTPS
   - `443/tcp` — the app itself
   - `3478/udp` and `3478/tcp` — TURN relay (needed when a peer is behind symmetric NAT / CGNAT)
3. **No CGNAT** on the server side. If your ISP puts you behind CGNAT, HTTP-01
   certificate issuance will fail and inbound calls won't reach you — host on a
   VPS instead.

## Getting the binary

Download from [GitHub Releases](https://github.com/Pinnss/pinTalk/releases):

| Asset | Platform |
|---|---|
| `pintalk-linux-amd64` | Ubuntu / Debian / any x86_64 Linux |
| `pintalk-linux-arm64` | ARM64: OpenWrt aarch64 (BPI-R3), Raspberry Pi 4/5 (64-bit OS), ARM servers |

The binaries are fully static (`CGO_ENABLED=0`), so the same file works on any
distro of the matching architecture — there is no separate Ubuntu vs Debian build.

Or build from source (Go 1.25+):

```bash
git clone https://github.com/Pinnss/pinTalk.git && cd pinTalk
CGO_ENABLED=0 go build -ldflags="-s -w" -o pintalk ./cmd/pintalk          # current platform
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o pintalk-linux-arm64 ./cmd/pintalk
```

## Configuration (both platforms)

Generate a bcrypt hash of your host password (any machine, even Windows):

```bash
./pintalk hash 'my-strong-password'
# → $2a$12$...
```

Create `config.yaml` from [`config.example.yaml`](../config.example.yaml):

```yaml
server:
  domain: call.example.com     # your domain
  listen_ip: ""                # "" = all interfaces; set to WAN IP if :80/:443 are taken on LAN
  http_port: 80
  https_port: 443
  cert_cache: /etc/pintalk/certs

turn:
  enabled: true
  listen_ip: 0.0.0.0
  port: 3478
  external_ip: ""              # WAN IP; "" lets it auto-resolve from the domain
  realm: call.example.com
  shared_secret: "<paste output of: openssl rand -hex 32>"
  cred_ttl_minutes: 60

hosts:
  - username: pin
    password_hash: "<paste the $2a$12$... hash from above>"
```

---

## Linux (Ubuntu / Debian)

Tested on Ubuntu 22.04/24.04 and Debian 12; any systemd distro works the same way.

### 1. Install the binary

```bash
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
pintalk --help   # sanity check
```

### 2. Create a service user and the config directory

```bash
sudo useradd --system --home /etc/pintalk --shell /usr/sbin/nologin pintalk
sudo mkdir -p /etc/pintalk/certs
sudo cp config.yaml /etc/pintalk/config.yaml
sudo chown -R pintalk:pintalk /etc/pintalk
sudo chmod 700 /etc/pintalk/certs
sudo chmod 600 /etc/pintalk/config.yaml
```

### 3. Install the systemd unit

Use [`deploy/systemd/pintalk.service`](../deploy/systemd/pintalk.service) from this repo:

```bash
sudo cp deploy/systemd/pintalk.service /etc/systemd/system/pintalk.service
sudo systemctl daemon-reload
sudo systemctl enable --now pintalk
```

The unit runs pintalk as the unprivileged `pintalk` user and grants
`CAP_NET_BIND_SERVICE` so it can bind ports 80/443 without root.

### 4. Open the firewall (if enabled)

```bash
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 3478/tcp
sudo ufw allow 3478/udp
```

### 5. Verify

```bash
systemctl status pintalk
journalctl -u pintalk -f
# expect:
#   [pintalk] TURN listening on 0.0.0.0:3478 ...
#   [pintalk] http listening on :80 (ACME + redirect)
#   [pintalk] https listening on :443 (domain=call.example.com)
```

Open `https://call.example.com` — the login form should appear. The very first
HTTPS request takes ~5–15 s while autocert obtains the certificate; it is then
cached in `/etc/pintalk/certs`.

### Updating

```bash
sudo systemctl stop pintalk
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
sudo systemctl start pintalk
```

---

## OpenWrt

Tested on OpenWrt 24.10 (BananaPi R3, aarch64). Uses the stock procd init system.

> If LuCI occupies `:80/:443` on the LAN interface, set `server.listen_ip` in
> `config.yaml` to the router's **WAN IP** so the two don't fight over ports.

### 1. Copy the files to the router

From your workstation (`ROUTER=root@192.168.1.1` — adjust):

```bash
ROUTER=root@192.168.1.1

# binary
scp pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk
ssh $ROUTER 'chmod +x /usr/bin/pintalk'

# config (with real secrets filled in)
ssh $ROUTER 'mkdir -p /etc/pintalk/certs && chmod 700 /etc/pintalk/certs'
scp config.yaml $ROUTER:/etc/pintalk/config.yaml
ssh $ROUTER 'chmod 600 /etc/pintalk/config.yaml'

# procd init script
scp deploy/init.d/pintalk $ROUTER:/etc/init.d/pintalk
ssh $ROUTER 'chmod +x /etc/init.d/pintalk'
```

### 2. Open the firewall

The repo ships a helper that adds the three rules via UCI:

```bash
scp deploy/firewall-add.sh $ROUTER:/tmp/
ssh $ROUTER 'sh /tmp/firewall-add.sh && rm /tmp/firewall-add.sh'
```

(Equivalent to allowing `80/tcp`, `443/tcp`, `3478/tcp+udp` from the WAN zone.)

### 3. Enable and start

```bash
ssh $ROUTER '/etc/init.d/pintalk enable && /etc/init.d/pintalk start'
```

### 4. Verify

```bash
ssh $ROUTER '/etc/init.d/pintalk status'
ssh $ROUTER 'logread -e pintalk | tail -30'
```

Then open `https://call.example.com` in a browser (first request is slow — see above).

### Updating

```bash
scp pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk.new
ssh $ROUTER 'mv /usr/bin/pintalk.new /usr/bin/pintalk && chmod +x /usr/bin/pintalk && /etc/init.d/pintalk restart'
```

(Or `make deploy ROUTER=root@...` if you work from a clone of this repo.)

---

## Troubleshooting

**Certificate is not issued.**
Check DNS: `dig +short call.example.com` must equal your WAN IP (`curl -s ifconfig.me`
from the server). Check that port 80 is reachable from the outside:
`curl -v http://call.example.com/.well-known/acme-challenge/test` should return 404
(served by pintalk itself). Behind CGNAT HTTP-01 cannot work.

**Calls connect on LAN but not across networks.**
The TURN port is probably closed — test with `nc -zv call.example.com 3478` from
outside. If the server is behind NAT, set `turn.external_ip` to the real WAN IP,
otherwise TURN hands out private-IP ICE candidates.

**Screen sharing button is missing.**
Screen capture requires a desktop browser (`getDisplayMedia`); on phones the
button is hidden by design.
