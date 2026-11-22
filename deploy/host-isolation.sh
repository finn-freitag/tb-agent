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
# Bridges of the "offline" network policy (tb-docker-off, tb-virt-off,
# tb-incus-off) get no forwarding at all and only DHCP from the node (no DNS),
# on top of what Docker/libvirt/Incus already enforce for them.
#
# Environment: TOSSBOX_BRIDGES (default "tb-docker virbr0 incusbr0"),
#              TOSSBOX_OFFLINE_BRIDGES (default "tb-docker-off tb-virt-off tb-incus-off"),
#              TOSSBOX_ALLOW_V4 / TOSSBOX_ALLOW_V6: destinations to allow
#              anyway (space separated CIDRs).
set -eu

BRIDGES=${TOSSBOX_BRIDGES:-"tb-docker virbr0 incusbr0"}
OFFLINE_BRIDGES=${TOSSBOX_OFFLINE_BRIDGES:-"tb-docker-off tb-virt-off tb-incus-off"}
ALLOW_V4=${TOSSBOX_ALLOW_V4:-}
ALLOW_V6=${TOSSBOX_ALLOW_V6:-}
BLOCK_V4="10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16 224.0.0.0/4"
BLOCK_V6="fc00::/7 fe80::/10 ff00::/8"

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }

# The rules live in the mangle table: it runs before the filter table, where
# Docker (DOCKER-FORWARD), Tailscale (ts-forward accepts everything leaving
# via tailscale0), ufw and libvirt insert their ACCEPT rules. ACCEPT/RETURN in
# mangle only end our checks; the filter rules still apply afterwards.
T="-t mangle"

# hook <cmd> <parent chain> <our chain>: (re)insert the jump as first rule.
hook() {
	while $1 $T -D "$2" -j "$3" 2>/dev/null; do :; done
	$1 $T -I "$2" 1 -j "$3"
}

unhook() {
	while $1 $T -D "$2" -j "$3" 2>/dev/null; do :; done
	$1 $T -F "$3" 2>/dev/null || true
	$1 $T -X "$3" 2>/dev/null || true
}

# Earlier versions used the filter table (DOCKER-USER / INPUT).
unhook_legacy() {
	for pair in DOCKER-USER:TOSSBOX-FWD INPUT:TOSSBOX-IN; do
		while $1 -D "${pair%%:*}" -j "${pair#*:}" 2>/dev/null; do :; done
		$1 -F "${pair#*:}" 2>/dev/null || true
		$1 -X "${pair#*:}" 2>/dev/null || true
	done
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
	unhook_legacy "$ipt"
	$ipt $T -N TOSSBOX-FWD 2>/dev/null || $ipt $T -F TOSSBOX-FWD
	$ipt $T -N TOSSBOX-IN 2>/dev/null || $ipt $T -F TOSSBOX-IN
	for br in $BRIDGES; do
		# Forwarded traffic from devices: replies are fine, private destinations aren't.
		$ipt $T -A TOSSBOX-FWD -i "$br" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
		for a in $allow; do $ipt $T -A TOSSBOX-FWD -i "$br" -d "$a" -j RETURN; done
		for b in $block; do $ipt $T -A TOSSBOX-FWD -i "$br" -d "$b" -j DROP; done
		# Traffic from devices to the node itself: only DHCP, DNS and ICMP.
		$ipt $T -A TOSSBOX-IN -i "$br" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
		$ipt $T -A TOSSBOX-IN -i "$br" -p udp --dport 53 -j RETURN
		$ipt $T -A TOSSBOX-IN -i "$br" -p tcp --dport 53 -j RETURN
		if [ "$fam" = v4 ]; then
			$ipt $T -A TOSSBOX-IN -i "$br" -p udp --dport 67 -j RETURN
			$ipt $T -A TOSSBOX-IN -i "$br" -p icmp -j RETURN
		else
			$ipt $T -A TOSSBOX-IN -i "$br" -p udp --dport 547 -j RETURN
			$ipt $T -A TOSSBOX-IN -i "$br" -p ipv6-icmp -j RETURN # NDP, router advertisements
		fi
		$ipt $T -A TOSSBOX-IN -i "$br" -j DROP
	done
	for br in $OFFLINE_BRIDGES; do
		# No forwarding in either direction; the node reaches devices directly.
		$ipt $T -A TOSSBOX-FWD -i "$br" -j DROP
		$ipt $T -A TOSSBOX-FWD -o "$br" -j DROP
		$ipt $T -A TOSSBOX-IN -i "$br" -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
		if [ "$fam" = v4 ]; then
			$ipt $T -A TOSSBOX-IN -i "$br" -p udp --dport 67 -j RETURN
		else
			$ipt $T -A TOSSBOX-IN -i "$br" -p ipv6-icmp -j RETURN
		fi
		$ipt $T -A TOSSBOX-IN -i "$br" -j DROP
	done
	hook "$ipt" FORWARD TOSSBOX-FWD
	hook "$ipt" INPUT TOSSBOX-IN
}

apply() {
	apply_family iptables "$BLOCK_V4" "$ALLOW_V4" v4
	if command -v ip6tables >/dev/null 2>&1; then
		apply_family ip6tables "$BLOCK_V6 $(v6_prefixes | tr '\n' ' ')" "$ALLOW_V6" v6
	fi
	echo "Tossbox isolation active for: $BRIDGES (offline: $OFFLINE_BRIDGES)"
}

remove() {
	for ipt in iptables ip6tables; do
		command -v $ipt >/dev/null 2>&1 || continue
		unhook $ipt FORWARD TOSSBOX-FWD
		unhook $ipt INPUT TOSSBOX-IN
		unhook_legacy $ipt
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
	echo "Installed tossbox-isolation.service"
}

case "${1:-apply}" in
	apply) apply ;;
	remove) remove ;;
	install) install_service ;;
	*) echo "usage: $0 [apply|install|remove]" >&2; exit 2 ;;
esac
