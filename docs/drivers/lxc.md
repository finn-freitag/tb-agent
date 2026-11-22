# lxc driver

Linux system containers, and optionally Incus VMs, through the [Incus](https://linuxcontainers.org/incus/) REST API on its unix socket.

Status: implemented and unit tested against a fake Incus API. **Not tested against a real Incus daemon yet.**

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
```

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
