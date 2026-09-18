package proc

import (
	"syscall"
	"testing"
)

func TestParseSignal(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  syscall.Signal
		err   bool
	}{
		{name: "numeric passthrough 9", token: "9", want: syscall.SIGKILL},
		{name: "numeric passthrough 15", token: "15", want: syscall.SIGTERM},
		{name: "numeric passthrough 1", token: "1", want: syscall.SIGHUP},
		{name: "bare upper name KILL", token: "KILL", want: syscall.SIGKILL},
		{name: "bare lower name term", token: "term", want: syscall.SIGTERM},
		{name: "bare mixed name QuIt", token: "QuIt", want: syscall.SIGQUIT},
		{name: "SIG-prefixed SIGHUP", token: "SIGHUP", want: syscall.SIGHUP},
		{name: "SIG-prefixed lowercase sigusr1", token: "sigusr1", want: syscall.SIGUSR1},
		{name: "SIG-prefixed mixed SigWinch", token: "SigWinch", want: syscall.SIGWINCH},
		{name: "CONT", token: "CONT", want: syscall.SIGCONT},
		{name: "STOP", token: "STOP", want: syscall.SIGSTOP},
		{name: "INT", token: "INT", want: syscall.SIGINT},
		{name: "unknown name FOO", token: "FOO", err: true},
		{name: "unknown name sigfoo", token: "sigfoo", err: true},
		{name: "unknown number 999", token: "999", err: true},
		{name: "zero number", token: "0", err: true},
		{name: "negative number", token: "-1", err: true},
		{name: "empty token", token: "", err: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseSignal(c.token)
			if c.err {
				if err == nil {
					t.Fatalf("ParseSignal(%q) succeeded (%v), want error", c.token, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSignal(%q) error: %v", c.token, err)
			}
			if got != c.want {
				t.Fatalf("ParseSignal(%q) = %v, want %v", c.token, got, c.want)
			}
		})
	}
}

func TestSignalTarget(t *testing.T) {
	cases := []struct {
		name    string
		pid     int
		pgid    int
		want    int
		wantErr bool
	}{
		{name: "real process group uses -pgid", pid: 4242, pgid: 4242, want: -4242},
		{name: "group differs from pid uses -pgid", pid: 100, pgid: 200, want: -200},
		// agy's defensive-bug case: pid == 1 with no group must target the process directly,
		// never -1 (which syscall.Kill treats as a broadcast to every reachable process).
		{name: "pid 1 with no pgid targets pid directly", pid: 1, pgid: 0, want: 1},
		{name: "no pgid falls back to direct pid", pid: 4242, pgid: 0, want: 4242},
		{name: "no pgid falls back negative pgid", pid: 4242, pgid: -5, want: 4242},
		{name: "pid 0 invalid", pid: 0, pgid: 0, wantErr: true},
		{name: "negative pid invalid", pid: -3, pgid: 0, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := signalTarget(c.pid, c.pgid)
			if c.wantErr {
				if err == nil {
					t.Fatalf("signalTarget(%d, %d) = %d, want error", c.pid, c.pgid, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("signalTarget(%d, %d) error: %v", c.pid, c.pgid, err)
			}
			if got != c.want {
				t.Fatalf("signalTarget(%d, %d) = %d, want %d", c.pid, c.pgid, got, c.want)
			}
		})
	}
}
