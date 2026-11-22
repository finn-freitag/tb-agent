# lxc driver

Linux system containers, and optionally Incus VMs, through the [Incus](https://linuxcontainers.org/incus/) REST API on its unix socket.

Status: tested against Incus 6.0.5 on Ubuntu 26.04 (Debian 13 and Alpine 3.24 cloud containers: boot, cloud-init, SSH through the gateway, delete).

**Storage limit:** the root disk `size` is only enforced by storage pools that support quotas (zfs, btrfs, lvm). On a `dir` pool (what `incus admin init --minimal` creates) the container sees the whole host filesystem.

**Network isolation:** see `isolate_ports` below and `deploy/host-isolation.sh`.

**Nesting:** Ubuntu 24.04+ hosts set `kernel.apparmor_restrict_unprivileged_userns=1`. systemd 257+ inside a container then hangs at boot (services stuck in `(sd-mkuserns)`, no DHCP lease) unless `security.nesting=true`. The driver therefore sets it on every container by default; override it via `container_config`.

## Config

```yaml
drivers:
  lxc:
    socket: /var/lib/incus/unix.socket
    project: default
    storage_pool: default
    profiles: [default]           # must provide a NIC (e.g. incusbr0)
    remotes:                      # used by incus:<remote>:<alias> image URLs
      images: https://images.linuxcontainers.org
    allow_vms: false              # allow templates with param lxc.type=virtual-machine
    stop_timeout: 30s
    container_config:             # extra Incus config for every container
      security.nesting: "true"    # default; needed by systemd 257+ on Ubuntu hosts
    isolate_ports: true           # default; devices can't reach each other
```

**Offline policy:** containers boot on the normal network first, so cloud-init can install packages (the SSH server). Once `/run/cloud-init/result.json` exists (or after `offline_provision_timeout`, default 15m), the agent moves their NICs to `tb-incus-off` (no NAT, no DNS) and restarts them.

**Isolation:** with `isolate_ports` (default), the driver copies the profiles' bridged NICs onto each instance with `security.port_isolation=true`. Devices on `incusbr0` then reach the host and the internet, but not each other (tested on Incus 6.0.5). macvlan or physical NICs can't be isolated this way and are left to the profile.

The agent needs access to the Incus socket (member of the `incus-admin` group, or mount the socket when running in a container).

## Mapping

- **Image:** `incus:images:debian/13/cloud` becomes source `{type: image, protocol: simplestreams, server: <remote URL>, alias: debian/13/cloud}`. Use `/cloud` variants: they contain cloud-init.
- **Provisioning:** `cloud-init.user-data` gets the shared `internal/cloudinit` Linux user data (hostname, default user with the generated password, SSH keys, password SSH login).
- **Resources:**
  - `limits.cpu` = vcpus
  - `limits.memory` = `<ram_mb>MiB`
  - root disk device `size` = `<storage_gb>GiB` in `storage_pool`
  - CPU cap % = `limits.cpu.allowance: "<cores×pct>ms/100ms"`. This is a hard limit expressed as a share of all host cores, and applies to containers only.
- **Ownership:** config keys `user.tossbox.device` and `user.tossbox.driver=lxc`. The instance name is `tb-<id>`.
- **Internal IP:** first global IPv4 from `GET /1.0/instances/<name>/state` (eth0 / enp5s0 preferred). Storage used comes from `disk.root.usage`.
- **Async operations** are awaited via `GET /1.0/operations/<id>/wait`.
- **Services:** ssh, http, https. vnc is offered only when `allow_vms` is set.

## Sources (verified 2026-11-22)

- lxc/incus `doc/rest-api.yaml`: `/1.0/instances`, `/1.0/instances/{name}/state`, `/1.0/operations/{id}/wait`, `InstanceSource`
- lxc/incus `doc/config_options.txt`: `cloud-init.user-data`, `limits.cpu`, `limits.cpu.allowance` (`50%` is a soft limit, `25ms/100ms` a hard one, containers only), `limits.memory`
