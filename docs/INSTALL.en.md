# pintalk — Installation Guide

*Читайте эту инструкцию на русском: [INSTALL.ru.md](INSTALL.ru.md).*

pintalk is a single static Go binary: web UI, signaling and TURN relay are all
built in. No Docker, no database, no external dependencies.

HTTPS is required (browsers won't grant camera/mic access without it), but a
domain is **not**. There are three certificate modes:

| `tls.mode` | When to use | Domain | Browser warning |
|---|---|---|---|
| `selfsigned` | LAN / by IP | not needed | yes (or install the CA once) |
| `letsencrypt-ip` | public IP, no domain | not needed | no (~6-day cert, auto-renewed) |
| `letsencrypt-domain` | you have a domain | needed | no |

---

## Option 1 — one-command installer (Linux + systemd)

The fastest path. Downloads the right binary, runs the setup wizard, installs a
systemd service and opens the firewall:

```bash
curl -fsSL https://raw.githubusercontent.com/Pinnss/pinTalk/main/deploy/install.sh | sudo sh
```

The wizard asks whether you have a domain / public IP, picks the mode for you,
generates a password (or takes yours) plus the secrets and certificate. Then open
the printed link and log in.

To upgrade, re-run the same command — your config is left in place.

---

## Option 2 — manual

### Step 1. Get the binary

Download from [GitHub Releases](https://github.com/Pinnss/pinTalk/releases):

| Asset | Platform |
|---|---|
| `pintalk-linux-amd64` | Ubuntu / Debian / any x86_64 Linux |
| `pintalk-linux-arm64` | ARM64: OpenWrt aarch64 (BPI-R3), Raspberry Pi 4/5, ARM servers |

The binaries are fully static (`CGO_ENABLED=0`) — one file per architecture works
on any distro. Or build from source (Go 1.25+):

```bash
git clone https://github.com/Pinnss/pinTalk.git && cd pinTalk
make build-amd64   # → bin/pintalk-linux-amd64
make build-arm64   # → bin/pintalk-linux-arm64
```

### Step 2. Generate the config

Easiest is the wizard (it hashes the password and creates the secrets for you):

```bash
pintalk init
```

It asks a few questions and writes `config.yaml`. Prefer to do it by hand? Copy
[`config.example.yaml`](../config.example.yaml). A minimal **LAN, no-domain** config:

```yaml
server:
  listen_ip: ""          # "" = all interfaces
  http_port: 80
  https_port: 443

tls:
  mode: selfsigned
  cert_cache: /etc/pintalk/certs

turn:
  enabled: false         # on a LAN, P2P connects directly — TURN not needed

hosts:
  - username: pin
    password_hash: "<output of: pintalk hash 'my-password'>"
```

For a **public IP without a domain**: `tls.mode: letsencrypt-ip`, `tls.public_ip:
<your IP>`, `turn.enabled: true`, `turn.external_ip: <your IP>`. For a **domain**:
`tls.mode: letsencrypt-domain`, `server.domain: call.example.com`.

### What each mode needs

- **selfsigned** — nothing external; works even offline. Port 443 (and 80, for the
  CA download + redirect) are served locally.
- **letsencrypt-domain** — the domain resolves to the server's WAN IP; `80/tcp`
  and `443/tcp` reachable from the internet; **no CGNAT** (ACME HTTP-01 needs it).
- **letsencrypt-ip** — a public IP; `80/tcp` and `443/tcp` reachable; the cert is
  short-lived (~6 days) and pintalk renews it automatically.
- **TURN** (`3478/tcp`+`3478/udp`) — needed when a peer is behind symmetric NAT /
  CGNAT. On a pure LAN you can skip it.

---

## Linux (Ubuntu / Debian, systemd) — manual

Tested on Ubuntu 22.04/24.04 and Debian 12.

### 1. Install the binary

```bash
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
pintalk --help
```

### 2. Config directory + service user

```bash
sudo useradd --system --home /etc/pintalk --shell /usr/sbin/nologin pintalk
sudo mkdir -p /etc/pintalk/certs
# generate the config straight into /etc/pintalk (absolute cert-cache for systemd):
sudo pintalk init --config /etc/pintalk/config.yaml --cert-cache /etc/pintalk/certs
sudo chown -R pintalk:pintalk /etc/pintalk
sudo chmod 700 /etc/pintalk/certs
sudo chmod 600 /etc/pintalk/config.yaml
```

### 3. systemd unit

```bash
sudo cp deploy/systemd/pintalk.service /etc/systemd/system/pintalk.service
sudo systemctl daemon-reload
sudo systemctl enable --now pintalk
```

The unit runs pintalk as the unprivileged `pintalk` user with
`CAP_NET_BIND_SERVICE` so it can bind 80/443 without root.

### 4. Firewall (for letsencrypt-* and TURN)

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
# self-signed:        [pintalk] https listening on :443 (self-signed TLS)
# letsencrypt-ip:     [pintalk] https listening on :443 (letsencrypt IP=<your IP>)
# letsencrypt-domain: [pintalk] https listening on :443 (letsencrypt domain=call.example.com)
```

Open the link the wizard printed. In `letsencrypt-*` modes the very first HTTPS
request takes ~5–15 s while the certificate is obtained.

### Updating

```bash
sudo systemctl stop pintalk
sudo install -m 755 pintalk-linux-amd64 /usr/local/bin/pintalk
sudo systemctl start pintalk
```

---

## OpenWrt

Tested on OpenWrt 24.10 (BananaPi R3, aarch64). Stock procd init.

> If LuCI occupies `:80/:443` on the LAN interface, set `server.listen_ip` in
> `config.yaml` to the router's **WAN IP**.

### 1. Copy the files to the router

```bash
ROUTER=root@192.168.1.1

scp pintalk-linux-arm64 $ROUTER:/usr/bin/pintalk
ssh $ROUTER 'chmod +x /usr/bin/pintalk'

# config: easiest to generate it locally with the wizard, then upload
pintalk init --config ./config.yaml --cert-cache /etc/pintalk/certs
ssh $ROUTER 'mkdir -p /etc/pintalk/certs && chmod 700 /etc/pintalk/certs'
scp config.yaml $ROUTER:/etc/pintalk/config.yaml
ssh $ROUTER 'chmod 600 /etc/pintalk/config.yaml'

scp deploy/init.d/pintalk $ROUTER:/etc/init.d/pintalk
ssh $ROUTER 'chmod +x /etc/init.d/pintalk'
```

### 2. Firewall (for letsencrypt-* / TURN)

```bash
scp deploy/firewall-add.sh $ROUTER:/tmp/
ssh $ROUTER 'sh /tmp/firewall-add.sh && rm /tmp/firewall-add.sh'
```

### 3. Enable and start

```bash
ssh $ROUTER '/etc/init.d/pintalk enable && /etc/init.d/pintalk start'
ssh $ROUTER 'logread -e pintalk | tail -30'
```

### Updating

```bash
make deploy ROUTER=root@192.168.2.1
```

---

## Troubleshooting

**Browser warns about the certificate (`selfsigned` mode).**
That's expected — the cert is self-signed. Click "Advanced → Proceed". To remove
the warning for good, open `http://<host>/pintalk-ca.crt`, download and install
that CA as trusted on each device. After that you get a green lock.

**The guest also has to click "Proceed".**
Yes — in `selfsigned` mode everyone who opens the link for the first time sees the
warning. Either share the CA file with them, or, for zero warnings, use a domain
(`letsencrypt-domain`) or get a cert for your public IP (`letsencrypt-ip`).

**Let's Encrypt certificate is not issued.**
Domain mode: `dig +short call.example.com` must equal your WAN IP
(`curl -s ifconfig.me` on the server), and `curl -v http://<host>/.well-known/acme-challenge/test`
from outside must reach pintalk (404). IP mode: the IP must be public and reachable
on port 80. Behind CGNAT HTTP-01 cannot work — use a VPS.

**Calls connect on LAN but not across networks.**
The TURN port is probably closed — test `nc -zv <host> 3478` from outside. If the
server is behind NAT, set `turn.external_ip` to the real WAN IP.

**Screen sharing button is missing.**
Screen capture requires a desktop browser (`getDisplayMedia`); on phones the
button is hidden by design.
