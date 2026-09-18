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
