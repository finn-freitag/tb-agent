# usb-phone driver (experimental)

Leases **physical** Android phones and iPhones attached over USB. Instead of being created and deleted, phones are leased and reset.

> **Experimental.** Untested on real hardware. Factory resets of real phones are inherently device-specific; test the reset flow on each model before trusting it.

## Requirements

- `adb` (Android platform-tools); `fastboot` only for the opt-in wipe.
- Optional for iPhones: libimobiledevice (`idevice_id`), plus your own reset script. Typical tools for supervised devices are `cfgutil erase` on macOS or `pymobiledevice3`.

## Config

```yaml
drivers:
  usb-phone:
    adb: adb
    fastboot: fastboot
    idevice_id: idevice_id
    forward_port_base: 15000
    command_timeout: 15m
    android:
      fastboot_wipe: false       # DANGEROUS, see below
      keep_packages: []
      setup_command: ""          # <cmd> <serial>, run after leasing
      reset_command: ""          # <cmd> <serial>, replaces the built-in reset
    ios:
      setup_command: ""          # <cmd> <udid>
      reset_command: ""          # <cmd> <udid>, required: iPhones aren't leased without it
```

## Behaviour

- **Capabilities:** attached phones from `adb devices -l` (USB only; network serials are skipped) and `idevice_id -l` become `physical_devices`. `available` means attached, authorized, not leased, and for iOS a reset command is configured. `fixed_slots` = number of attached phones.
- **Ensure (lease):** picks the phone from param `serial`, or any free phone of the template family, and persists the lease in `<data>/leases/<id>.json`. Then it runs the setup:
  - Android with the adb service: `adb -s S tcpip 5555` and `adb wait-for-device`, then the optional `setup_command`.
  - iOS: the optional `setup_command`.
- **adb exposure:** `adb forward tcp:<forward_port_base+slot> tcp:5555`. The driver reports `127.0.0.1:<port>` through `driver.EndpointResolver`, and the agent core forwards the device's public adb port there. The forward is removed when the device is stopped or destroyed, and recreated after an agent restart.
- **Power:** phones stay on. "Stopped" removes the adb forward (the endpoint is reported as unreachable) and sends `KEYCODE_SLEEP`.
- **Destroy (reset + release):**
  - **iOS:** `reset_command <udid>`.
  - **Android:** `reset_command <serial>` if set. Otherwise, with `fastboot_wipe: true`: `adb reboot bootloader`, `fastboot -w`, `fastboot reboot`. A wipe disables USB debugging, so the phone comes back unusable for adb unless its image enables adb by default, and setup needs a custom flow. Otherwise the built-in cleanup runs: uninstall all third-party packages except `keep_packages`, empty the common `/sdcard` folders, `adb usb`.
- **Status:** a leased but disconnected phone is reported as `FAILED` ("phone … is disconnected").
