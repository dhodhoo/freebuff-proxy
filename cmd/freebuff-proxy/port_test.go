package main

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestIsPortInUse(t *testing.T) {
	// A bind failure surfaces as *net.OpError → *os.SyscallError → Errno,
	// exactly what errors.Is unwraps.
	real := &net.OpError{Op: "listen", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}}
	if !isPortInUse(real) {
		t.Error("isPortInUse(EADDRINUSE chain) = false, want true")
	}
	if isPortInUse(errors.New("boom")) {
		t.Error("isPortInUse(random) = true, want false")
	}
	// Windows: WSAEADDRINUSE surfaces as a plain Errno whose string differs
	// from syscall.EADDRINUSE — matched by message fallback.
	if !isPortInUse(errors.New("bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.")) {
		t.Error("isPortInUse(win bind msg) = false, want true")
	}
	if !isPortInUse(errors.New("listen tcp 127.0.0.1:3457: bind: address already in use")) {
		t.Error("isPortInUse(linux bind msg) = false, want true")
	}
	if isPortInUse(nil) {
		t.Error("isPortInUse(nil) = true, want false")
	}
}

func TestPortOf(t *testing.T) {
	cases := []struct{ addr, want string }{
		{":3457", "3457"},
		{"127.0.0.1:3457", "3457"},
		{"[::1]:8080", "8080"},
		{"", ""},
		{"no-port", ""},
	}
	for _, c := range cases {
		if got := portOf(c.addr); got != c.want {
			t.Errorf("portOf(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}

func TestWindowsPortPIDFromOutput(t *testing.T) {
	out := `  TCP    127.0.0.1:3456         0.0.0.0:0              LISTENING       111
  TCP    127.0.0.1:3457         0.0.0.0:0              LISTENING       44420
  TCP    127.0.0.1:3457         0.0.0.0:0              ESTABLISHED     9999
`
	if got := windowsPortPIDFromOutput(out, "3457"); got != "44420" {
		t.Errorf("windowsPortPIDFromOutput(3457) = %q, want 44420", got)
	}
	// Port with no listener → empty.
	if got := windowsPortPIDFromOutput(out, "9999"); got != "" {
		t.Errorf("windowsPortPIDFromOutput(9999) = %q, want empty", got)
	}
	if got := windowsPortPIDFromOutput("", "3457"); got != "" {
		t.Errorf("windowsPortPIDFromOutput(empty) = %q, want empty", got)
	}
}

func TestTaskNameFromCSV(t *testing.T) {
	if got := taskNameFromCSV(`"freebuff-proxy-dash.exe","44420","Console","1","50,776 K"`); got != "freebuff-proxy-dash.exe" {
		t.Errorf("taskNameFromCSV = %q, want freebuff-proxy-dash.exe", got)
	}
	if got := taskNameFromCSV("no quotes"); got != "" {
		t.Errorf("taskNameFromCSV(no quotes) = %q, want empty", got)
	}
	if got := taskNameFromCSV(""); got != "" {
		t.Errorf("taskNameFromCSV(empty) = %q, want empty", got)
	}
}
