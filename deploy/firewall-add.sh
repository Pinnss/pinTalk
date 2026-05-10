#!/bin/sh
# Adds OpenWrt firewall rules so pintalk can accept inbound HTTP(S) and TURN.
# Idempotent: re-running won't add duplicates.

set -e

add_rule() {
    name="$1"; proto="$2"; port="$3"
    if uci show firewall | grep -q "name='$name'"; then
        echo "rule '$name' already exists, skipping"
        return
    fi
    uci batch <<EOF
add firewall rule
set firewall.@rule[-1].name='$name'
set firewall.@rule[-1].src='wan'
set firewall.@rule[-1].proto='$proto'
set firewall.@rule[-1].dest_port='$port'
set firewall.@rule[-1].target='ACCEPT'
EOF
    echo "added rule: $name ($proto/$port)"
}

add_rule 'Allow-pintalk-http'  'tcp'      '80'
add_rule 'Allow-pintalk-https' 'tcp'      '443'
add_rule 'Allow-pintalk-turn'  'tcp udp'  '3478'

uci commit firewall
/etc/init.d/firewall reload
echo "done."
