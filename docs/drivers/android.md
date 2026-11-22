# android driver

Virtual Android devices with two sub-backends:

- **redroid:** Android in a privileged Docker container.
- **cuttlefish:** Google's official KVM virtual device.

The backend is chosen by the device param `backend`, otherwise the template param `android.backend`, otherwise `default_backend`. An existing device always stays on the backend it was created with.

Status: **redroid tested** (Android 15 `_64only` image on Ubuntu 26.04, kernel 7.0 with binderfs only, i.e. no `/dev/binder` nodes: boot, adb through the gateway, internet, delete). Cuttlefish is unit tested only (fake `launch_cvd`).

## Config

```yaml
drivers:
  android:
    default_backend: redroid
    redroid:
      enabled: true
      socket: /var/run/docker.sock
      network: bridge
      image: redroid/redroid:15.0.0_64only-latest   # used if the template has no docker:// image
      data_dir: /var/lib/tb-agent/android/redroid    # per device /data
      host_data_dir: ""
      width: 720
      height: 1280
      dpi: 320
      gpu_mode: guest            # guest (software) | host | auto
      use_memfd: true            # androidboot.use_memfd=true: no ashmem module needed
    cuttlefish:
      enabled: false
      host_package_dir: /opt/cuttlefish/host   # extracted cvd-host_package.tar.gz
      images_dir: /opt/cuttlefish/images       # extracted aosp_cf_*-img-*.zip
      runtime_dir: /var/lib/tb-agent/android/cuttlefish
      max_instances: 4
      extra_args: []
```

If neither backend is enabled explicitly, redroid is enabled. Only healthy backends are reported in the capabilities' `backends`.

## redroid

- **Host requirements** (from the redroid docs):
  - binder support: `modprobe binder_linux devices="binder,hwbinder,vndbinder"` or binderfs
  - ashmem: not needed, because `use_memfd` is the default here
  - the container runs `--privileged`

  The driver reports itself unhealthy if `/proc/filesystems` has no binder and `/dev/binder` is missing.
- **Container:** name `tb-<id>`, labels `tossbox.device`, `tossbox.driver=android`, `tossbox.android-backend=redroid`.
  - `/data` is bind mounted from `<data_dir>/<id>`.
  - Command line: `androidboot.redroid_width/height/dpi/gpu_mode`, plus `androidboot.use_memfd=true`.
  - Template params `redroid.width`, `redroid.height`, `redroid.dpi` override the config.
  - Memory limit = ram_mb. CPU = vcpus, or the cap % of host cores.
- **Access:** adb on `<container IP>:5555`. redroid has no VNC; use scrcpy over adb.
- **Warning:** adb has no authentication. Expose it publicly only for throwaway devices, and prefer the `tailscale` or `none` exposure.

## Cuttlefish

- One instance per device, each with its own `HOME` (`<runtime_dir>/<id>`) that symlinks the host package's entries. Instance numbers 1..`max_instances` are allocated and persisted in `<home>/instance`.
- **Launch:** `launch_cvd --daemon --base_instance_num=N --num_instances=1 --system_image_dir=<images> --cpus --memory_mb --report_anonymous_usage_stats=n`, plus:
  - first boot: `--data_policy=always_create --blank_data_image_mb=<storage>`
  - later boots: `--data_policy=use_existing`
  - when the vnc service is enabled: `--start_vnc_server=true`
- **Stop** uses `stop_cvd`.
- **Images:** template image `local:<dir>` (format `dir`) is resolved relative to `images_dir` (absolute paths are allowed). Without one, `images_dir` itself is used. Fetch the images once per node, e.g.:

  ```bash
  cvd fetch --default_build=aosp-android-latest-release/aosp_cf_x86_64_only_phone-userdebug \
    --target_directory=/opt/cuttlefish/images/aosp_cf_x86_64_only_phone
  ```

  The same download provides the host package; point `host_package_dir` at it (it must contain `bin/launch_cvd`). The `android-cuttlefish` template in tb-templates expects `local:aosp_cf_x86_64_only_phone`.
- **Ports:** instance N listens on adb `127.0.0.1:6520+N-1` and VNC `127.0.0.1:6444+N-1`. The driver reports these through `driver.EndpointResolver`, so the agent core forwards the device's public adb/vnc ports straight to them. No loopback aliases are involved, so this works on any host.
- **Running state** is tracked with a `running` marker file. A crashed launcher isn't detected until adb readiness fails.

## Sources (verified 2026-11-22)

- remote-android/redroid-doc `README.md`: image tags (Android 8.1–16, `_64only` variants), `binder_linux` / `ashmem_linux` modules, `--privileged`, `-v ~/data:/data`, port 5555, the `androidboot.*` parameter table
- google/android-cuttlefish `host/commands/start/start_flags.cpp`: `--base_instance_num`, `--num_instances` (`--report_anonymous_usage_stats` is deprecated and ignored); `docs/GettingStarted.md`: adb on `localhost:6520` for instance 1; `--start_vnc_server` and `--blank_data_image_mb` exist in the repo's config and flags sources
- **Not verified this session:** `--daemon`, `--data_policy` values, and the VNC base port 6444. Check them on a real host.
