# tb-agent

Node agent for [Tossbox](https://github.com/finn-freitag/tossbox), the self-hosted service for throwaway devices.

One agent runs per host (a homelab server, a spare PC, a Mac mini, a Pi with phones attached). It connects **outbound** to the dashboard, receives the desired state of its devices, and makes it happen through one or more **drivers**:

| Driver | Runs | Host | Status |
|---|---|---|---|
| `libvirt` | KVM VMs via the host's libvirt (Linux, Windows) | Linux | implemented, tested against real libvirt |
| `docker-qemu` | one container per VM (QEMU in Docker, e.g. dockur/windows) | Linux + Docker | tested (Linux + Windows guests) |
| `lxc` | Linux system containers via Incus | Linux + Incus | tested with Incus 6.0 |
| `android` | redroid / Cuttlefish | Linux | redroid tested; Cuttlefish unit tested |
| `orchard` | macOS VMs via Orchard/Tart (2 per Mac) | macOS | see `internal/driver/orchard` |
| `usb-phone` | physical phones over USB | Linux/macOS | see `internal/driver/usbphone` |

What the agent core does for every driver:
- **Enrollment and control stream:** a one-time token is exchanged for an mTLS client certificate. Then a long-lived gRPC stream ([tb-proto](https://github.com/finn-freitag/tb-proto)) carries Hello, Welcome, ApplyDevice/DeleteDevice, DeviceStatus and heartbeats.
- **Reconciler:** keeps the desired spec of every device, persisted in `data_dir/state.json`, so devices survive agent restarts and dashboard outages. Calls `Ensure`/`Destroy` and retries with backoff.
- **Readiness checks:** SSH banner, or a TCP connect for RDP/ADB, against the device's internal address.
- **Port forwarding:** a userspace TCP proxy from `listen_address:<public_port>` to the device's service. The dashboard gateway uses the same port number.
- **License pools:** Windows product keys from the node config are leased to devices, persisted in `data_dir/licenses.json`. Keys never leave the node.
- **Image cache:** `https://` images are downloaded once (resumable, sha256-verified) to `data_dir/images/cache`. `local:<file>` images are read from `data_dir/images/`.

## Install

First create the node in the dashboard (**Nodes → Add node**, with the address where the dashboard can reach this host, e.g. its Tailscale IP). Copy the enrollment token, then write a config based on [`examples/node.yaml`](examples/node.yaml).

The node must accept connections from the dashboard host on the device port range (default `40000-49999`), and must be able to reach the dashboard's gRPC port (default `7443`).

### Linux node with Docker (recommended)

```bash
mkdir tb-agent && cd tb-agent
curl -fsSLO https://raw.githubusercontent.com/finn-freitag/tb-agent/main/docker-compose.yml
curl -fsSL https://raw.githubusercontent.com/finn-freitag/tb-agent/main/examples/node.yaml -o node.yaml
$EDITOR node.yaml
docker compose up -d
docker compose logs -f
```

The container uses host networking (forwarded ports open on the host) and talks to the host's libvirt through the mounted socket. VMs run in the host's libvirt, not in the container. For the other drivers, uncomment the matching socket or device mounts in `docker-compose.yml`.

### Linux node with systemd

```bash
sudo curl -fsSL -o /usr/local/bin/tb-agent https://github.com/finn-freitag/tb-agent/releases/latest/download/tb-agent-linux-amd64
sudo chmod +x /usr/local/bin/tb-agent
sudo mkdir -p /etc/tb-agent && sudo $EDITOR /etc/tb-agent/node.yaml
sudo curl -fsSL -o /etc/systemd/system/tb-agent.service https://raw.githubusercontent.com/finn-freitag/tb-agent/main/deploy/tb-agent.service
sudo systemctl daemon-reload && sudo systemctl enable --now tb-agent
```

### macOS node (Orchard)

Docker on macOS cannot reach Virtualization.framework, so the agent runs natively:

```bash
sudo curl -fsSL -o /usr/local/bin/tb-agent https://github.com/finn-freitag/tb-agent/releases/latest/download/tb-agent-darwin-arm64
sudo chmod +x /usr/local/bin/tb-agent
sudo mkdir -p /usr/local/etc/tb-agent /usr/local/var/lib/tb-agent /usr/local/var/log
sudo $EDITOR /usr/local/etc/tb-agent/node.yaml     # set data_dir: /usr/local/var/lib/tb-agent
sudo curl -fsSL -o /Library/LaunchDaemons/com.tossbox.agent.plist https://raw.githubusercontent.com/finn-freitag/tb-agent/main/deploy/com.tossbox.agent.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/com.tossbox.agent.plist
```

### Check a config

```bash
tb-agent check -config /etc/tb-agent/node.yaml
```

This prints the parsed config, whether the dashboard is reachable, and what each driver reports (capabilities, health).

## libvirt driver

Requirements on the host: libvirt with QEMU/KVM, a libvirt network (default `default`) and a storage pool (default `default`).

**Storage:** everything is created through the libvirt API in the configured storage pool. The agent needs no filesystem access to the pool, which keeps it container friendly:
- `tb-base-<hash>.img`: base image, uploaded once per template image and shared as the qcow2 backing file.
- `tb-<device>.qcow2`: the device's linked-clone overlay, sized to the requested storage.
- `tb-<device>-cidata.iso`: NoCloud seed for cloud-init (Linux) or cloudbase-init (Windows).

**Domains** are named `tb-<device id>` and carry a `<tossbox:device>` metadata element. Only such domains are ever touched; other VMs on the host are left alone. Destroying a device removes the domain, its overlay, seed ISO, NVRAM and TPM state. Base images stay cached in the pool; delete unused `tb-base-*` volumes manually if needed.

**Guests:**
- **Linux** (`cloud-init`): hostname, the template's default user with the generated password and passwordless sudo, your SSH keys, password SSH login, qemu-guest-agent.
- **Windows** (`cloudbase-init`): the golden image (built with Packer from [tb-templates](https://github.com/finn-freitag/tb-templates)) must contain cloudbase-init, VirtIO drivers and OpenSSH. The seed sets the user and password, enables RDP with NLA and OpenSSH, and installs and activates the product key from the chosen license option. Windows VMs get UEFI with Secure Boot (libvirt firmware auto-selection, or explicit `windows.loader`/`nvram_template`) and a TPM 2.0 (swtpm), both configurable.

**CPU cap:** `cpu_cap_percent` (share of the whole host) becomes a cgroup `global_quota` on the domain.

**VNC:** each VM has a VNC console served by QEMU on the host (`vnc_listen`, default `127.0.0.1`, autoport). Its password is the device password truncated to 8 characters, which is a VNC protocol limitation. The driver implements the optional `ServiceEndpoint` extension, so the agent forwards the `vnc` service to that console rather than to the guest.

**Networking:** the VM's IP comes from the libvirt network's DHCP leases, or the guest agent when using `bridge`. The agent forwards public ports to `<vm ip>:<internal port>`.

## Development

```bash
nix-shell                     # Go toolchain
go build ./... && go vet ./... && go test -race ./...

# Real libvirt test: creates and destroys a Debian 13 VM (768 MB) on qemu:///system
TB_IT_DATA_DIR=/tmp/tb-it go test -tags integration -run TestLibvirt -v -timeout 30m ./internal/agent
```

Layout:

```
cmd/tb-agent          entrypoint: run | check | version
internal/agent        enrollment, control stream, reconciler, port forwarding, readiness
internal/config       node.yaml parsing and validation
internal/driver       driver interface + registry; one package per driver
internal/images       image download cache
internal/licenses     Windows license key leasing
internal/cloudinit    NoCloud seed ISOs (cloud-init / cloudbase-init)
internal/hostinfo     host CPU/RAM/storage
deploy/               systemd unit, launchd plist
```

Each driver registers itself in `init()` (`driver.Register`). Which drivers are active is decided by the `drivers:` section of the node config.
