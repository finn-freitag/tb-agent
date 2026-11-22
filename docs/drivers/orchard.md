# orchard driver

macOS VMs (Tart) through an [Orchard](https://github.com/cirruslabs/orchard) controller.

Status: implemented and unit tested against a fake Orchard API. **Not tested against a real controller yet.**

## Topology

There are two ways to reach a VM's services:

- **VM IP (default):** the device's internal IP is the VM's address on its worker (`GET /v1/vms/{name}/ip`), which only the worker host can reach. Run `tb-agent` natively (launchd) on the worker Mac; one node = one Mac.
- **Worker endpoints (`use_endpoints: true`):** every enabled service is declared as an Orchard `endpoint` (a port on the worker forwarding to the VM's internal port). The driver reports `<worker address>:<workerPort>` from `observedEndpoints` through `driver.EndpointResolver`. The agent can then run on any host that reaches the workers, e.g. next to the controller. This needs workers that support endpoint forwarding.

## Config

```yaml
drivers:
  orchard:
    url: http://127.0.0.1:6120
    service_account_name: tossbox     # needs compute:read + compute:write
    service_account_token: <token>
    ca_file: ""                       # PEM CA if the controller uses a self-signed cert
    insecure_skip_verify: false
    slots: 0                          # 0 = sum of the workers' org.cirruslabs.tart-vms (default 2 per worker)
    image_username: admin             # credentials baked into the images (Cirrus Labs images: admin/admin)
    image_password: admin
    labels: {}                        # worker labels new VMs require
    allow_linux: false
    use_endpoints: false
    worker_addresses: {}              # worker name -> address reachable from the agent
    default_worker_address: 127.0.0.1
```

## Mapping

- `POST /v1/vms` with:
  - `name: tb-<id>`
  - `os: darwin`
  - `image` (template `oci://<ref>` without the scheme)
  - `cpu`, `memory` (MiB), `diskSize` (GB)
  - `headless: true`, `restart_policy: Never`
  - `username`/`password`: the **image** credentials Orchard uses for SSH
  - `startup_script`
- **Startup script:** Orchard runs it over SSH after boot. It sets the hostname, sets the generated password for the device user (or creates that user with `sysadminctl` if it doesn't exist) and installs the SSH keys. Template params `orchard.image_username` / `orchard.image_password` override the image credentials.
- **Auth:** HTTP Basic with the service account name and token. All paths are under `/v1`.
- **Status:** `status` pending → provisioning, running → running, failed → failed; `powerState: stopped` → stopped.
- **Slots:** `fixed_slots` = configured `slots`, otherwise the sum of the workers' `org.cirruslabs.tart-vms` resource, otherwise 2 per worker.

## Limitations

- **Power:** Orchard can move a VM into `powerState: stopped` only once and can't start it again. Switching a device off works; switching it back on fails with a clear error, and the device has to be recreated.
- **Stale Orchard credentials:** once the startup script has changed the image user's password, Orchard's stored credentials no longer work. Later Orchard `exec` / startup scripts (e.g. after a VM restart) would fail. With `restart_policy: Never` this only matters for manual restarts.
- **Endpoints are fixed at creation:** with `use_endpoints`, services are declared when the VM is created and aren't updated later.

## Sources (verified 2026-11-22)

- cirruslabs/orchard `api/openapi.yaml`: `EndpointSpec`/`EndpointStatus` (`workerPort`, `state: listening`), `/vms`, `/vms/{name}`, `/vms/{name}/ip`, `/workers`; VMSpec fields (`os`, `image`, `cpu`, `memory`, `diskSize`, `username`, `password`, `startup_script`, `restart_policy`, `labels`, `headless`, `powerState` with its one-way stop note); VMState `status` enum
- `pkg/client/client.go`: `/v1` prefix, Basic auth with the service account name and token
- `pkg/resource/v1/resources.go`: `org.cirruslabs.tart-vms`, `org.cirruslabs.logical-cores`, `org.cirruslabs.memory-mib`
