# docker-qemu driver

Runs one Docker container per VM, with QEMU inside the container:

- **Windows:** [dockur/windows](https://github.com/dockur/windows) (`dockurr/windows`)
- **Linux and other disk images:** [qemus/qemu](https://github.com/qemus/qemu) (`qemux/qemu`)

Status: tested on a real Docker 29 host (Ubuntu 26.04): Debian, Fedora and Windows 11 (dockurr) guests.

## Requirements

- Docker Engine reachable via its socket.
- `/dev/kvm` and `/dev/net/tun` on the host. The driver passes both into the container together with `NET_ADMIN`, as both projects require.
- If the agent itself runs in a container, mount the Docker socket and the storage dir. Set `host_storage_dir` when the storage dir has a different path on the host.

## Config

```yaml
drivers:
  docker-qemu:
    socket: /var/run/docker.sock
    network: bridge                 # the device's internal IP is the container IP on this network
    storage_dir: /var/lib/tb-agent/docker-qemu/devices
    host_storage_dir: ""            # storage_dir as seen by the Docker daemon
    prepared_dir: /var/lib/tb-agent/docker-qemu/prepared
    windows_image: dockurr/windows
    linux_image: qemux/qemu
    stop_timeout: 120s
```

## Mapping

| Spec | Container |
|---|---|
| `resources.vcpus` | `CPU_CORES` |
| `resources.ram_mb` | `RAM_SIZE` (`8G` or `1536M`) |
| `resources.storage_gb` | `DISK_SIZE` (`150G`) |
| `resources.cpu_cap_percent` | `NanoCpus` (cap as a share of all host cores) |
| Windows user / password | `USERNAME` / `PASSWORD` (dockur creates the account during the unattended install) |
| Windows product key (own key or pool) | `KEY` |
| hostname | `HOST` (Windows computer name) and container hostname |
| `ssh` service on Windows | `COMMAND` installs and starts the OpenSSH server at the end of the install |
| per device storage | `<storage_dir>/<id>/storage` bind mounted to `/storage` |
| labels | `tossbox.device=<id>`, `tossbox.driver=docker-qemu`, `tossbox.template` |

Both images NAT the guest behind the container IP and forward every port except their own web (8006) and VNC (5900) ports. The agent core reaches `rdp` / `ssh` / `http(s)` at `<container IP>:<internal port>`. `vnc` (5900) reaches QEMU's display server in the container, so it works even before the guest has networking.

## Template params

| Param | Meaning |
|---|---|
| `docker_qemu.version` | dockur `VERSION` (e.g. `11`, `2025`, or an ISO URL) |
| `docker_qemu.edition`, `docker_qemu.language` | dockur `EDITION`, `LANGUAGE` |
| `docker_qemu.prepared_storage` | `local:<dir>` (relative to `prepared_dir`): a copy of a fully installed dockur `/storage` directory, copied per device to skip the 10–20 min install. **Limitation:** the account and password are the ones from when the snapshot was made; the generated password is not applied. |
| `docker_qemu.boot_image` | Linux: `https://` cloud image (qcow2/img) fetched through the agent's image cache, copied per device and mounted as `/boot.<ext>`. Defaults to the template image URL if that is not `docker://`. |
| `docker_qemu.boot_image_sha256` | optional checksum for the boot image |
| `docker_qemu.boot` | qemux built-in OS name (e.g. `ubuntu`), used when there is no boot image. No provisioning happens in this case. |

Linux provisioning uses the shared `internal/cloudinit` NoCloud seed. It is written to `<dir>/seed.iso` and attached via `ARGUMENTS=-drive file=/seed.iso,format=raw,if=virtio,readonly=on`. The boot disk is grown with `qemu-img resize` when `qemu-img` is installed in the agent's environment; otherwise it keeps the cloud image's size.

## Sources (verified 2026-11-22)

- dockur/windows `readme.md` and `docs/environment.md`: `VERSION`, `EDITION`, `LANGUAGE`, `KEY`, `USERNAME`, `PASSWORD`, `HOST`, `RAM_SIZE`, `CPU_CORES`, `DISK_SIZE`, `COMMAND`, `HOST_PORTS`, `/dev/kvm`, `/dev/net/tun`, `NET_ADMIN`, `stop_grace_period: 2m`
- qemus/qemu `readme.md`, `docs/environment.md`, `src/install.sh`: `BOOT`, `/boot.{img,raw,iso,qcow2}` bind mounts, `ARGUMENTS`
