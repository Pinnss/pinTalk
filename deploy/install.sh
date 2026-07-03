#!/bin/sh
# pintalk one-command installer for Linux (systemd).
#
#   curl -fsSL https://raw.githubusercontent.com/Pinnss/pinTalk/main/deploy/install.sh | sudo sh
#
# It downloads the right binary, creates a service user, runs the setup wizard
# (`pintalk init`), installs a systemd unit, opens the firewall, and starts the
# service. Re-running it upgrades the binary and leaves your config in place.
#
# Env overrides: PINTALK_VERSION (default: latest), PINTALK_PREFIX (/usr/local/bin).
set -eu

REPO="Pinnss/pinTalk"
PREFIX="${PINTALK_PREFIX:-/usr/local/bin}"
CONF_DIR="/etc/pintalk"
CONF="$CONF_DIR/config.yaml"
CERT_DIR="$CONF_DIR/certs"
UNIT="/etc/systemd/system/pintalk.service"
VERSION="${PINTALK_VERSION:-latest}"

log()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root (prefix with sudo)"
command -v systemctl >/dev/null 2>&1 || die "this installer targets systemd distros; see docs/INSTALL for OpenWrt / others"

# --- pick the binary for this architecture ---
case "$(uname -m)" in
	x86_64|amd64)         ASSET="pintalk-linux-amd64" ;;
	aarch64|arm64)        ASSET="pintalk-linux-arm64" ;;
	*) die "unsupported architecture $(uname -m) — build from source (see docs/INSTALL)";;
esac

if [ "$VERSION" = latest ]; then
	URL="https://github.com/$REPO/releases/latest/download/$ASSET"
else
	URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
fi

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then curl -fSL --proto '=https' --proto-redir '=https' -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then wget --https-only -O "$2" "$1"
	else die "need curl or wget"; fi
}

sha256_of() { # file -> hex digest on stdout, or non-zero if no tool
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
	else return 1; fi
}

log "Downloading $ASSET ($VERSION)"
TMP="$(mktemp)"
fetch "$URL" "$TMP" || die "download failed from $URL"

# Verify the download against the release's SHA256SUMS when it is published.
if [ "$VERSION" = latest ]; then
	SUMS_URL="https://github.com/$REPO/releases/latest/download/SHA256SUMS"
else
	SUMS_URL="https://github.com/$REPO/releases/download/$VERSION/SHA256SUMS"
fi
SUMS="$(mktemp)"
if fetch "$SUMS_URL" "$SUMS" 2>/dev/null; then
	expected="$(awk -v a="$ASSET" '$2==a || $2=="*"a {print $1}' "$SUMS" | head -n1)"
	[ -n "$expected" ] || { rm -f "$TMP" "$SUMS"; die "no checksum for $ASSET in SHA256SUMS"; }
	actual="$(sha256_of "$TMP")" || { rm -f "$TMP" "$SUMS"; die "need sha256sum or shasum to verify the download"; }
	[ "$expected" = "$actual" ] || { rm -f "$TMP" "$SUMS"; die "checksum mismatch for $ASSET (expected $expected, got $actual)"; }
	log "Checksum verified"
else
	warn "no SHA256SUMS published for this release — skipping checksum verification"
fi
rm -f "$SUMS"

install -m 0755 "$TMP" "$PREFIX/pintalk"
rm -f "$TMP"

# Hard gate: a corrupt or wrong-architecture binary must not reach the service.
"$PREFIX/pintalk" --help >/dev/null 2>&1 || die "installed binary failed to run (corrupt download or wrong architecture)"
log "Installed $PREFIX/pintalk"

# --- service user + directories ---
if ! id pintalk >/dev/null 2>&1; then
	useradd --system --home "$CONF_DIR" --shell /usr/sbin/nologin pintalk
fi
mkdir -p "$CERT_DIR"

# --- config (only on first install; keep an existing one) ---
if [ -f "$CONF" ]; then
	log "Existing config kept: $CONF"
else
	if ( exec 3<>/dev/tty ) 2>/dev/null; then
		log "Setup wizard"
		"$PREFIX/pintalk" init --config "$CONF" --cert-cache "$CERT_DIR" </dev/tty >/dev/tty 2>&1 \
			|| die "setup wizard failed"
	else
		warn "no terminal for the wizard; writing a self-signed LAN config with a generated password"
		"$PREFIX/pintalk" init --yes --config "$CONF" --cert-cache "$CERT_DIR"
		warn "review $CONF, then: systemctl restart pintalk"
	fi
fi

chown -R pintalk:pintalk "$CONF_DIR"
chmod 600 "$CONF"
chmod 700 "$CERT_DIR"

# --- systemd unit ---
log "Installing systemd unit"
cat > "$UNIT" <<'UNITEOF'
[Unit]
Description=pintalk — 1-on-1 WebRTC calls (signaling + TURN + HTTPS)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/pintalk serve --config /etc/pintalk/config.yaml
WorkingDirectory=/etc/pintalk
Restart=on-failure
RestartSec=5
User=pintalk
Group=pintalk

# Bind :80/:443/:3478 without running as root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true

# Hardening: the only place pintalk writes is the certificate cache.
ProtectSystem=strict
ReadWritePaths=/etc/pintalk/certs
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNITEOF

# If the installed binary lives somewhere other than the default, fix ExecStart.
if [ "$PREFIX/pintalk" != "/usr/local/bin/pintalk" ]; then
	sed -i "s#/usr/local/bin/pintalk#$PREFIX/pintalk#" "$UNIT"
fi

systemctl daemon-reload
systemctl enable pintalk >/dev/null 2>&1 || true

# --- firewall (best effort) ---
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi "status: active"; then
	log "Opening firewall (ufw): 80, 443, 3478"
	ufw allow 80/tcp   >/dev/null 2>&1 || true
	ufw allow 443/tcp  >/dev/null 2>&1 || true
	ufw allow 3478/tcp >/dev/null 2>&1 || true
	ufw allow 3478/udp >/dev/null 2>&1 || true
fi

log "Starting pintalk"
systemctl restart pintalk

sleep 1
if systemctl is-active --quiet pintalk; then
	log "pintalk is running. Logs: journalctl -u pintalk -f"
else
	warn "pintalk did not start cleanly. Check: journalctl -u pintalk -e"
fi
