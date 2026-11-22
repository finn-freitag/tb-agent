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
- **Port forwarding:** a userspace TCP proxy from `listen_address:<public_port>` to the device's service. The dashboard gateway uses the same port number. Only sources in `allowed_sources` (default: the dashboard and private/Tailscale ranges) are forwarded, so the gateway's allowlists and rate limits can't be bypassed from the internet.
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

**Networking:** the VM's IP comes from the libvirt network's DHCP leases, or the guest agent when using `bridge`. The agent forwards public ports to `<vm ip>:<internal port>`. Each VM's bridge port is isolated (`<port isolated='yes'/>`, `isolate_ports: true` by default), so VMs on the same network can't reach each other.

## Device isolation

Devices of different users share a node, so the agent keeps them apart:

- **Device to device:** the Docker network `tossbox` has inter-container communication off, libvirt VMs get isolated bridge ports, and Incus NICs use `security.port_isolation`.
- **Device to node and LAN:** [`deploy/host-isolation.sh`](deploy/host-isolation.sh) adds firewall rules (run once as root, `install` also sets up a systemd unit). Devices then only reach the internet, plus DHCP/DNS on the node. Otherwise a device could reach every service on the node's LAN, e.g. a router or Home Assistant.
- **Inbound:** forwarded device ports only accept the dashboard and private networks (`allowed_sources`).
- **adb:** redroid devices require adb authentication with the owner's keys (see `docs/drivers/android.md`).

- **Network policy per device:** `internet` (default; with `deploy/host-isolation.sh` private networks are blocked) or `offline`. Offline devices sit on separate networks without any forwarding: the Docker network `tossbox-offline` (internal), the libvirt network `tossbox-offline` (no `<forward>`, no DNS) and the Incus bridge `tb-incus-off` (no NAT). They have no internet and no DNS, but users still reach them through their forwarded services and Guacamole, because those connections start on the node. Supported by docker-qemu, android (redroid), browser, libvirt (network mode) and lxc.

## GPU sharing

Devices can ask for a GPU (`gpu: true` in the API, "Share the node's GPU" in the UI). How they get one depends on the driver:

| Driver | Mode | How |
|---|---|---|
| libvirt | `virgl` | `drivers.libvirt.gpu: virgl`: shared paravirtual 3D (virtio-gpu with VirGL on the node's render node). Linux and ChromeOS guests, unlimited devices. Needs the EGL libraries on the host: `sudo sh deploy/host-gpu.sh virgl` |
| libvirt | `sriov` | `drivers.libvirt.gpu: sriov` + `gpu.sriov.pf`: each device gets its own SR-IOV virtual function (PCI passthrough), so as many devices as VFs. Intel iGPUs need the xe driver in SR-IOV mode: `sudo sh deploy/host-gpu.sh sriov 7` (reboot; experimental, `revert` undoes it) |
| docker-qemu | `render` | qemux renders the guest's virtio-gpu with VirGL (`GPU=Y`, `/dev/dri`); Linux guests |
| android (redroid) | `render` | the device renders on the host GPU (`androidboot.redroid_gpu_mode=host`) |
| lxc | `render` | an Incus `gpu` device (the container sees the host's render node) |

The agent container needs `/dev/dri` (see `docker-compose.yml`) to find the render node. Templates can mark a GPU as `required` or `recommended` (param `gpu`). Admins can limit devices with a GPU per user (quota `max_gpus`).

## Disposable browsers

The `browser` driver runs web browsers in containers (templates `browser-firefox`, `browser-chromium`, based on jlesage images): VNC on 5900 (password = device password, used by Guacamole) and a web UI on 5800 behind a login page (device user + password). Nothing persists: deleting the device deletes the profile, history and downloads. Containers run with `no-new-privileges`, a PID limit and a memory limit. The container disk isn't limited by `storage_gb`.

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
