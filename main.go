package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/coreos/go-systemd/v22/dbus"
)

type syncState struct {
	server string
	ready  bool
}

var prefix = regexp.MustCompile(`^\[[^\]]+\] `)
var serverEvent = regexp.MustCompile(`^SERVER: (\S+) \(.*\) (.*)$`)

func (s *syncState) consume(message string) {
	message = prefix.ReplaceAllString(message, "")
	m := serverEvent.FindStringSubmatch(message)
	if m != nil {
		if m[2] == "has connected to the network (uplinked to no uplink)" {
			s.server = m[1]
			s.ready = false
		} else if m[1] == s.server && m[2] == "is done syncing" {
			s.ready = true
		}

		return
	}

	for _, p := range []string{
		"Attempting to connect to uplink #",
		"Lost connection from uplink #",
		"Unable to connect to uplink #",
	} {
		if strings.HasPrefix(message, p) {
			s.ready = false
		}
	}
}

func (s *syncState) consumeJournal(data []byte, announced bool) (string, error) {
	cursor := ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var row struct {
			Message string `json:"MESSAGE"`
			Cursor  string `json:"__CURSOR"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.Message == "" || row.Cursor == "" {
			return "", errors.New("cannot decode Anope synchronization event")
		}
		s.consume(row.Message)
		if announced && !s.ready {
			return "", errors.New("Anope lost synchronization")
		}
		cursor = row.Cursor
	}
	return cursor, nil
}

func run(args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	b, e := c.Output()
	return b, stderr.Bytes(), e
}

func activeInvocation(unit string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := dbus.NewWithContext(ctx)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	properties, err := connection.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return "", err
	}
	if properties["ActiveState"] != "active" {
		return "", nil
	}
	invocation, ok := properties["InvocationID"].([]byte)
	if !ok || len(invocation) != 16 {
		return "", errors.New("invalid Anope invocation ID")
	}
	return hex.EncodeToString(invocation), nil
}

func notify(message string) error {
	sent, err := daemon.SdNotify(false, message)
	if err == nil && !sent {
		return errors.New("missing NOTIFY_SOCKET")
	}
	return err
}

func monitor(unit string) error {
	invocation, cursor := "", ""
	state := syncState{}
	announced := false
	for {
		current, e := activeInvocation(unit)
		if e != nil {
			return e
		}

		if current != invocation {
			if announced {
				return errors.New("Anope restarted after becoming ready")
			}

			invocation = current
			state = syncState{}
			cursor = ""
		}

		if invocation != "" {
			args := []string{
				"journalctl",
				"--unit=" + unit,
				"_SYSTEMD_INVOCATION_ID=" + invocation,
				"--output=json",
				"--quiet",
				"--no-pager",
				"--grep=SERVER:|Attempting to connect to uplink|Lost connection from uplink|Unable to connect to uplink",
			}
			if cursor != "" {
				args = append(args, "--after-cursor="+cursor)
			}

			b, stderr, e := run(args...)
			var exit *exec.ExitError
			if (e != nil && (!errors.As(e, &exit) || exit.ExitCode() != 1)) || len(stderr) > 0 {
				return errors.New("cannot read Anope synchronization events")
			}

			latest, e := state.consumeJournal(b, announced)
			if e != nil {
				return e
			}
			if latest != "" {
				cursor = latest
			}
		}

		current, e = activeInvocation(unit)
		if e != nil {
			return e
		}

		ready := invocation != "" && state.ready && current == invocation
		if announced && !ready {
			return errors.New("Anope lost synchronization")
		}

		if ready && !announced {
			if e = notify("READY=1"); e != nil {
				return e
			}

			announced = true
		}

		if e = notify("WATCHDOG=1"); e != nil {
			return e
		}

		time.Sleep(time.Second)
	}
}

var serviceUnit = regexp.MustCompile(`^(?:[A-Za-z0-9:_.-]|\\x[0-9A-Fa-f]{2})+(?:@(?:[A-Za-z0-9:_.-]|\\x[0-9A-Fa-f]{2})+)?\.service$`)

func parseUnit(args []string, output io.Writer) (string, error) {
	flags := flag.NewFlagSet("anope-systemd-ready", flag.ContinueOnError)
	flags.SetOutput(output)
	unit := flags.String("unit", "anope.service", "Monitor this systemd service unit")
	flags.Usage = func() {
		fmt.Fprintln(output, "Usage: anope-systemd-ready [--unit=anope.service]")
		fmt.Fprintln(output, "Monitor Anope synchronization and notify systemd of readiness")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", errors.New("unexpected positional arguments")
	}
	if len(*unit) > 255 || !serviceUnit.MatchString(*unit) {
		return "", errors.New("unit must be a concrete systemd .service name without paths or wildcards")
	}
	return *unit, nil
}

func main() {
	var usage bytes.Buffer
	unit, err := parseUnit(os.Args[1:], &usage)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Print(usage.String())
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	if err := monitor(unit); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
