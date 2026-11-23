#!/bin/sh
# Tossbox node: GPU sharing for devices.
#
#   sudo sh host-gpu.sh virgl         Shared 3D for VMs (VirGL): installs the EGL
#                                     libraries QEMU needs. No reboot.
#   sudo sh host-gpu.sh sriov [N]     Intel iGPU SR-IOV (12th gen and newer):
#                                     switches the GPU to the xe driver with N
#                                     virtual functions (default 7) that the agent
#                                     hands to VMs. Needs a reboot. EXPERIMENTAL:
#                                     the xe driver's SR-IOV support for older
#                                     generations depends on the kernel.
#   sudo sh host-gpu.sh sriov-status  Shows driver, VFs and IOMMU state.
#   sudo sh host-gpu.sh revert        Undo the SR-IOV switch (reboot afterwards).
#
# Environment: TOSSBOX_GPU_PF (PCI address, default 0000:00:02.0).
set -eu
PF=${TOSSBOX_GPU_PF:-0000:00:02.0}
SYS=/sys/bus/pci/devices/$PF
GRUB_CFG=/etc/default/grub.d/90-tossbox-gpu.cfg
UNIT=/etc/systemd/system/tossbox-gpu-vfs.service

need_root() { [ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }; }

# drv <sysfs device dir>: bound driver name or "none".
drv() { if [ -e "$1/driver" ]; then basename "$(readlink -f "$1/driver")"; else echo none; fi; }

status() {
	echo "PF $PF: $(lspci -nns "${PF#0000:}" 2>/dev/null | cut -d' ' -f2-)"
	echo "driver: $(drv "$SYS")"
	echo "VFs: $(cat "$SYS/sriov_numvfs" 2>/dev/null || echo -) of $(cat "$SYS/sriov_totalvfs" 2>/dev/null || echo -)"
	for v in "$SYS"/virtfn*; do
		[ -e "$v" ] || continue
		a=$(basename "$(readlink -f "$v")")
		g=$(basename "$(readlink -f "/sys/bus/pci/devices/$a/iommu_group" 2>/dev/null)" 2>/dev/null)
		echo "  $a driver=$(drv "/sys/bus/pci/devices/$a") iommu_group=${g:-none}"
	done
	echo "IOMMU groups: $(ls /sys/kernel/iommu_groups 2>/dev/null | wc -l)"
	echo "kernel cmdline: $(cat /proc/cmdline)"
}

case "${1:-}" in
virgl)
	need_root
	apt-get update -q
	DEBIAN_FRONTEND=noninteractive apt-get install -y -q libegl1 libegl-mesa0 libgbm1 libvirglrenderer1
	echo "VirGL libraries installed. Set drivers.libvirt.gpu: virgl in node.yaml."
	;;
sriov)
	need_root
	N=${2:-7}
	[ -e "$SYS/sriov_totalvfs" ] || { echo "$PF does not support SR-IOV" >&2; exit 1; }
	TOTAL=$(cat "$SYS/sriov_totalvfs")
	[ "$N" -ge 1 ] && [ "$N" -le "$TOTAL" ] || { echo "N must be 1..$TOTAL" >&2; exit 1; }
	ID=$(sed 's/^0x//' "$SYS/device")
	mkdir -p /etc/default/grub.d
	cat > "$GRUB_CFG" <<X
# Tossbox GPU SR-IOV (remove this file and run update-grub to undo)
GRUB_CMDLINE_LINUX_DEFAULT="\$GRUB_CMDLINE_LINUX_DEFAULT intel_iommu=on iommu=pt i915.force_probe=!$ID xe.force_probe=$ID xe.max_vfs=$N"
X
	echo vfio-pci > /etc/modules-load.d/tossbox-vfio.conf
	cat > "$UNIT" <<X
[Unit]
Description=Tossbox: create GPU SR-IOV virtual functions on $PF
After=systemd-modules-load.service
Before=libvirtd.service docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
# VFs are bound to vfio-pci (never to xe on the host): libvirt VMs and
# docker-qemu containers pass them through with VFIO.
ExecStart=/bin/sh -c 'for i in \$(seq 30); do [ -e $SYS/sriov_numvfs ] && [ -e $SYS/driver ] && break; sleep 1; done; echo 0 > $SYS/sriov_drivers_autoprobe; [ "\$(cat $SYS/sriov_numvfs)" = $N ] || echo $N > $SYS/sriov_numvfs; modprobe vfio-pci; for v in $SYS/virtfn*; do a=\$(basename \$(readlink -f \$v)); echo vfio-pci > /sys/bus/pci/devices/\$a/driver_override; [ -e /sys/bus/pci/devices/\$a/driver ] || echo \$a > /sys/bus/pci/drivers_probe; done'
ExecStop=/bin/sh -c 'echo 0 > $SYS/sriov_numvfs'

[Install]
WantedBy=multi-user.target
X
	systemctl daemon-reload
	systemctl enable tossbox-gpu-vfs.service
	update-grub
	echo
	if [ "$(basename "$(readlink -f "$SYS/driver" 2>/dev/null)")" = xe ] && [ "$(cat "$SYS/sriov_numvfs")" -gt 0 ]; then
		systemctl restart tossbox-gpu-vfs.service
		echo "SR-IOV already active: VFs re-created and bound to vfio-pci (no reboot needed)."
	else
		echo "Reboot to switch $PF to the xe driver with $N VFs. Afterwards check: sudo sh $0 sriov-status"
	fi
	echo "Undo: sudo sh $0 revert && reboot"
	;;
sriov-status)
	status
	;;
revert)
	need_root
	systemctl disable --now tossbox-gpu-vfs.service 2>/dev/null || true
	rm -f "$UNIT" "$GRUB_CFG" /etc/modules-load.d/tossbox-vfio.conf
	systemctl daemon-reload
	update-grub
	echo "SR-IOV setup removed; reboot to return to the i915 driver."
	;;
*)
	sed -n '2,17p' "$0"
	exit 2
	;;
esac
