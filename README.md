# Anope systemd readiness

`anope-systemd-ready` reads Anope synchronization messages from the systemd
journal, sends `READY=1` when the current uplink finishes syncing, and sends
`WATCHDOG=1` each poll. It fails if Anope stops, restarts, or loses synchronization
after readiness, allowing systemd to stop dependent services and run cleanup hooks.

The monitor selects journal entries from the service's current invocation and
checks that invocation again before announcing readiness. Raw journal messages
and `journalctl` diagnostics are not printed on failures.

## Requirements and build

- Linux with systemd, its system bus, and `journalctl` on `PATH`.
- Permission to read the unit's journal and query its systemd properties.
- Anope synchronization messages written to the journal. The event parser expects
  `SERVER: ... has connected to the network (uplinked to no uplink)` followed by
  `SERVER: ... is done syncing`, plus Anope's uplink reconnect messages. Optional
  bracketed log prefixes are supported. Enable these messages in Anope's logging
  configuration.
- Go 1.25.8 or newer to build.

```sh
go build -o anope-systemd-ready .
go test ./...
go vet ./...
./anope-systemd-ready --help
```

`--unit` selects the service; the default is `anope.service`.
The name must identify a concrete `.service` unit. Paths, patterns,
uninstantiated templates, and positional arguments are rejected. Help exits
successfully, invalid arguments exit with status 2, and runtime failures exit
with status 1.

## systemd service

Install the binary at `/usr/local/bin/anope-systemd-ready`, create an
`anope-ready` system account, and adapt this unit to your service and account
configuration. The account needs access to the system bus and journal:

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

The monitor polls once per second, with a five-second timeout for each of two
system-bus queries and one journal read. Allow for those operations when choosing
the watchdog interval. Missing synchronization events leave startup waiting until
`TimeoutStartSec`. The monitor requires systemd's `NOTIFY_SOCKET`.

Consumer services that must stop when this readiness service stops can use
`BindsTo=anope-ready.service` with `After=anope-ready.service`.

For actions triggered by readiness, add `ExecStartPost=` to the monitor's unit;
`Type=notify` runs it after `READY=1`. Pair it with an idempotent `ExecStopPost=`
that reverses the action after startup failure or monitor exit. Configure hook
permissions in the consuming unit; the monitor performs no deployment or network
changes.

Automatic restarts can announce readiness again after successful resynchronization.
