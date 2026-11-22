#!/bin/sh
# Tossbox node firewall: keep devices away from the host and the local network.
#
# Devices (VMs, containers, Android) run on these bridges:
#   tb-docker  Docker network "tossbox" (docker-qemu, android/redroid)
#   virbr0     libvirt network "default"
#   incusbr0   Incus bridge (lxc driver)
# The drivers already stop devices from talking to each other (Docker ICC off,
# isolated bridge ports). This script additionally drops traffic from those
# bridges to
#   - the node itself (except DHCP/DNS, which libvirt/Incus guests need), and
#   - private/local networks (RFC 1918, CGNAT incl. Tailscale, link-local,
#     IPv6 ULA and the node's own global IPv6 prefixes),
# so devices only get the internet. Traffic the node starts towards devices
# (the agent's port forwarder, readiness checks) keeps working.
#
# Usage:  sudo sh host-isolation.sh            apply now
#         sudo sh host-isolation.sh install    apply now and on every boot
#         sudo sh host-isolation.sh remove     undo
# Environment: TOSSBOX_BRIDGES (default "tb-docker virbr0 incusbr0"),
#              TOSSBOX_ALLOW_V4 / TOSSBOX_ALLOW_V6: destinations to allow
#              anyway (space separated CIDRs).
set -eu

BRIDGES=${TOSSBOX_BRIDGES:-"tb-docker virbr0 incusbr0"}
ALLOW_V4=${TOSSBOX_ALLOW_V4:-}
ALLOW_V6=${TOSSBOX_ALLOW_V6:-}
BLOCK_V4="10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 224.0.0.0/4"
BLOCK_V6="fc00::/7 fe80::/10 ff00::/8"

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }

# hook <cmd> <parent chain> <our chain>: (re)insert the jump as first rule.
hook() {
	while $1 -D "$2" -j "$3" 2>/dev/null; do :; done
	$1 -I "$2" 1 -j "$3"
}

unhook() {
	while $1 -D "$2" -j "$3" 2>/dev/null; do :; done
	$1 -F "$3" 2>/dev/null || true
	$1 -X "$3" 2>/dev/null || true
}

# Global IPv6 prefixes of the node (the LAN), excluding the device bridges.
v6_prefixes() {
	ip -6 -o addr show scope global 2>/dev/null | while read -r _ dev _ addr _; do
		case " $BRIDGES " in *" $dev "*) continue ;; esac
		case "$addr" in fc*|fd*) continue ;; esac # ULA, already blocked
		python3 -c 'import ipaddress,sys; print(ipaddress.ip_interface(sys.argv[1]).network)' "$addr" 2>/dev/null || true
	done | sort -u
}

apply_family() { # <iptables cmd> <block list> <allow list> <v4|v6>
	ipt=$1 block=$2 allow=$3 fam=$4
	$ipt -N TOSSBOX-FWD 2>/dev/null || $ipt -F TOSSBOX-FWD
	$ipt -N TOSSBOX-IN 2>/dev/null || $ipt -F TOSSBOX-IN
	for br in $BRIDGES; do
		# Forwarded traffic from devices: replies are fine, private destinations aren't.
		$ipt -A TOSSBOX-FWD -i "$br" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
		for a in $allow; do $ipt -A TOSSBOX-FWD -i "$br" -d "$a" -j RETURN; done
		for b in $block; do $ipt -A TOSSBOX-FWD -i "$br" -d "$b" -j DROP; done
		# Traffic from devices to the node itself: only DHCP, DNS and ICMP.
		$ipt -A TOSSBOX-IN -i "$br" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
		$ipt -A TOSSBOX-IN -i "$br" -p udp --dport 53 -j ACCEPT
		$ipt -A TOSSBOX-IN -i "$br" -p tcp --dport 53 -j ACCEPT
		if [ "$fam" = v4 ]; then
			$ipt -A TOSSBOX-IN -i "$br" -p udp --dport 67 -j ACCEPT
			$ipt -A TOSSBOX-IN -i "$br" -p icmp -j ACCEPT
		else
			$ipt -A TOSSBOX-IN -i "$br" -p udp --dport 547 -j ACCEPT
			$ipt -A TOSSBOX-IN -i "$br" -p ipv6-icmp -j ACCEPT # NDP, router advertisements
		fi
		$ipt -A TOSSBOX-IN -i "$br" -j DROP
	done
	# Docker evaluates DOCKER-USER first in FORWARD and leaves it alone.
	$ipt -N DOCKER-USER 2>/dev/null && $ipt -I FORWARD 1 -j DOCKER-USER || true
	hook "$ipt" DOCKER-USER TOSSBOX-FWD
	hook "$ipt" INPUT TOSSBOX-IN
}

apply() {
	apply_family iptables "$BLOCK_V4" "$ALLOW_V4" v4
	if command -v ip6tables >/dev/null 2>&1; then
		apply_family ip6tables "$BLOCK_V6 $(v6_prefixes | tr '\n' ' ')" "$ALLOW_V6" v6
	fi
	echo "Tossbox isolation active for: $BRIDGES"
}

remove() {
	for ipt in iptables ip6tables; do
		command -v $ipt >/dev/null 2>&1 || continue
		unhook $ipt DOCKER-USER TOSSBOX-FWD
		unhook $ipt INPUT TOSSBOX-IN
	done
	echo "Tossbox isolation removed"
}

install_service() {
	install -m 0755 "$0" /usr/local/sbin/tossbox-isolation
	cat > /etc/systemd/system/tossbox-isolation.service <<'X'
[Unit]
Description=Tossbox: keep devices away from the node and the local network
After=network-online.target docker.service libvirtd.service incus.service tossbox-bridges.service
Wants=network-online.target
PartOf=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/tossbox-isolation
ExecStop=/usr/local/sbin/tossbox-isolation remove

[Install]
WantedBy=multi-user.target docker.service
X
	systemctl daemon-reload
	systemctl enable tossbox-isolation.service
	systemctl restart tossbox-isolation.service
	# Runs after tossbox-bridges (ACCEPT rules for virbr0/incusbr0 in
	# DOCKER-USER) and re-inserts its jump as the first rule, so the drops win.
	echo "Installed tossbox-isolation.service"
}

case "${1:-apply}" in
	apply) apply ;;
	remove) remove ;;
	install) install_service ;;
	*) echo "usage: $0 [apply|install|remove]" >&2; exit 2 ;;
esac
