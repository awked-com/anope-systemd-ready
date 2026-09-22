package main

import (
	"bytes"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadJournal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		script  string
		want    string
		wantErr bool
	}{
		{name: "events", script: "printf events", want: "events"},
		{name: "no matches", script: "exit 1"},
		{name: "failed", script: "printf private-log-value; exit 2", wantErr: true},
		{name: "diagnostic", script: "printf private-log-value >&2", wantErr: true},
		{name: "failed with diagnostic", script: "printf private-log-value >&2; exit 1", wantErr: true},
		{name: "missing command", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			t.Setenv("PATH", bin)
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(bin, "journalctl"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readJournal("anope.service", "invocation", "")
			if string(got) != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("journal = %q, err = %v", got, err)
			}
			if err != nil && err.Error() != "cannot read Anope synchronization events" {
				t.Fatalf("journal diagnostics exposed: %v", err)
			}
		})
	}
}

func TestNotifications(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notify("READY=1"); err == nil {
		t.Fatal("missing notification socket accepted")
	}
	// Nix build directories can exceed the Unix socket path limit.
	t.Chdir(t.TempDir())
	socket, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: "notify", Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	t.Setenv("NOTIFY_SOCKET", "notify")
	if err := socket.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"READY=1", "WATCHDOG=1"} {
		if err := notify(message); err != nil {
			t.Fatal(err)
		}
		var buffer [64]byte
		n, err := socket.Read(buffer[:])
		if err != nil || string(buffer[:n]) != message {
			t.Fatalf("notification = %q, %v", buffer[:n], err)
		}
	}
	socket.Close()
	if err := notify("WATCHDOG=1"); err == nil {
		t.Fatal("failed notification accepted")
	}
}

func TestUnitSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "default", want: "anope.service"},
		{name: "named", args: []string{"--unit=services.service"}, want: "services.service"},
		{name: "instance", args: []string{"--unit", "anope@testing.service"}, want: "anope@testing.service"},
		{name: "escaped", args: []string{`--unit=anope\x2dtesting.service`}, want: `anope\x2dtesting.service`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			got, err := parseUnit(tc.args, &output)
			if err != nil || got != tc.want || output.Len() != 0 {
				t.Fatalf("unit = %q, err = %v, output = %q", got, err, output.String())
			}
		})
	}
	for _, args := range [][]string{
		{"--unknown"}, {"--unit"}, {"extra"}, {"--unit=anope.service", "extra"},
		{"--unit="}, {"--unit=*.service"}, {"--unit=anope?.service"},
		{"--unit=/etc/systemd/system/anope.service"}, {"--unit=anope.service\n"},
		{"--unit=anope.socket"}, {"--unit=anope@.service"}, {"--unit=.service"},
		{"--unit=@testing.service"}, {"--unit=anope@one@two.service"},
		{`--unit=anope\xZZ.service`}, {"--unit=" + strings.Repeat("a", 248) + ".service"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output bytes.Buffer
			if _, err := parseUnit(args, &output); err == nil {
				t.Fatal("invalid arguments accepted")
			}
		})
	}
	var output bytes.Buffer
	if _, err := parseUnit([]string{"--help"}, &output); !errors.Is(err, flag.ErrHelp) || output.Len() == 0 {
		t.Fatalf("help: %v, %q", err, output.String())
	}
}

func TestReconnectClearsReadiness(t *testing.T) {
	for _, event := range []string{
		"Attempting to connect to uplink #1",
		"Lost connection from uplink #1",
		"Unable to connect to uplink #1",
		"SERVER: replacement (description) has connected to the network (uplinked to no uplink)",
	} {
		t.Run(event, func(t *testing.T) {
			s := syncState{server: "uplink", ready: true}
			s.consume(event)
			if s.ready {
				t.Fatal("readiness remained set after a reconnect event")
			}
		})
	}
}

func TestJournalReadiness(t *testing.T) {
	batch := []byte(`{"MESSAGE":"Lost connection from uplink #1","__CURSOR":"first"}
{"MESSAGE":"SERVER: uplink (description) has connected to the network (uplinked to no uplink)","__CURSOR":"second"}
{"MESSAGE":"SERVER: uplink (description) is done syncing","__CURSOR":"third"}
`)
	s := syncState{}
	cursor, err := s.consumeJournal(batch, false)
	if err != nil || cursor != "third" || !s.ready {
		t.Fatalf("startup synchronization: cursor = %q, state = %+v, err = %v", cursor, s, err)
	}
	// A disconnect after READY must fail even if a later event in the same
	// polling batch already reports successful resynchronization.
	if _, err := s.consumeJournal(batch, true); err == nil {
		t.Fatal("disconnect after readiness accepted")
	}
	s = syncState{server: "uplink", ready: true}
	if cursor, err := s.consumeJournal(nil, true); err != nil || cursor != "" || !s.ready {
		t.Fatalf("empty batch changed readiness: cursor = %q, state = %+v, err = %v", cursor, s, err)
	}
}

func TestMalformedJournalSuppressesMessages(t *testing.T) {
	for _, data := range []string{
		`{"MESSAGE": "private-log-value",`,
		`{"MESSAGE": ["private-log-value"], "__CURSOR": "cursor"}`,
		`{"MESSAGE": "private-log-value"}`,
		`{"__CURSOR": "cursor"}`,
	} {
		s := syncState{}
		if _, err := s.consumeJournal([]byte(data), false); err == nil || strings.Contains(err.Error(), "private-log-value") {
			t.Fatalf("malformed journal error = %v", err)
		}
	}
}

func TestUplinkSynchronization(t *testing.T) {
	s := syncState{}
	s.consume("[time] SERVER: uplink (description) has connected to the network (uplinked to no uplink)")
	s.consume("SERVER: unrelated (description) is done syncing")
	if s.ready {
		t.Fatal("unrelated sync opened readiness")
	}

	s.consume("SERVER: uplink (description) is done syncing")
	if !s.ready {
		t.Fatal("uplink sync not recognized")
	}

	s.consume("Lost connection from uplink #1")
	if s.ready {
		t.Fatal("disconnect retained readiness")
	}

	s.consume("SERVER: other (description) has connected to the network (uplinked to no uplink)")
	s.consume("SERVER: uplink (description) is done syncing")
	if s.ready {
		t.Fatal("old uplink sync opened readiness")
	}
}
