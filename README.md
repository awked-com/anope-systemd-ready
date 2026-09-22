# Anope systemd readiness

`anope-systemd-ready` observes Anope synchronization messages in the systemd
journal. It sends `READY=1` after the current uplink has finished syncing and
continues sending `WATCHDOG=1`. It exits with an error if Anope stops, restarts,
or loses synchronization after readiness, so systemd can stop dependent services
and run consumer cleanup hooks.

The monitor selects journal entries from the service's current invocation and
checks that invocation again before announcing readiness. Raw journal messages
and `journalctl` diagnostics are not printed on failures.

## Requirements and build

- Linux with systemd, its system bus, and `journalctl` available on `PATH`.
- Permission to read the monitored unit's journal and query its systemd
  properties. A dedicated service account in `systemd-journal` is one option.
- Anope synchronization messages written to the journal. The event parser expects
  `SERVER: ... has connected to the network (uplinked to no uplink)` followed by
  `SERVER: ... is done syncing`, plus Anope's uplink reconnect messages. Optional
  bracketed log prefixes are supported. Confirm these messages are enabled for
  your Anope version and logging configuration.
- Go 1.25.8 or newer to build.

```sh
go build -o anope-systemd-ready .
go test ./...
go vet ./...
./anope-systemd-ready --help
```

Use `--unit=anope.service` to select the service to observe; that is also the
default. The name must identify a concrete `.service` unit. Paths, patterns,
uninstantiated templates, and positional arguments are rejected. Help exits
successfully, invalid arguments exit with status 2, and runtime failures exit
with status 1.

## Example systemd service

Install the binary at `/usr/local/bin/anope-systemd-ready`, create an
`anope-ready` system account, and adapt this unit to your service and account
configuration:

```ini
[Unit]
Description=Wait for Anope synchronization
Requires=anope.service
After=anope.service

[Service]
Type=notify
NotifyAccess=main
User=anope-ready
SupplementaryGroups=systemd-journal
ExecStart=/usr/local/bin/anope-systemd-ready --unit=anope.service
TimeoutStartSec=120s
WatchdogSec=30s
Restart=on-failure
RestartSec=5s
```

The monitor polls once per second; each system-bus or journal operation has a
five-second timeout. Keep the watchdog interval comfortably above those combined
timeouts. Startup requires the needed synchronization events to remain in the
journal; missing or disabled events leave the unit waiting until its startup
timeout. Invoking the monitor outside a notifying service fails because it needs
systemd's `NOTIFY_SOCKET`.

Consumer services that must stop when this readiness service stops can use
`BindsTo=anope-ready.service` with `After=anope-ready.service`.

If readiness should trigger privileged deployment actions, add `ExecStartPost=`
to this monitor's systemd unit in your consuming configuration. With
`Type=notify`, systemd runs that hook after receiving `READY=1`. Supply
appropriate permissions for the hook in your local unit configuration. Pair it
with an idempotent `ExecStopPost=` hook
that reverses the action even after startup failure or monitor exit. Keep those
scripts, their arguments, and their privilege policy in the consuming
configuration. The monitor itself performs no deployment or network changes.

Automatic restarts may observe a later successful resynchronization and announce
readiness again. Choose restart and dependency behavior to match your service's
recovery policy.
