package main

import "testing"

// `serve` is deliberately not exercised here: it binds a port and blocks. The
// routing decisions around it are what this test covers.
func TestDispatchRoutesNonServingCommands(t *testing.T) {
	tests := map[string]struct {
		args []string
		want int
	}{
		"help":       {[]string{"help"}, exitOK},
		"-h":         {[]string{"-h"}, exitOK},
		"--help":     {[]string{"--help"}, exitOK},
		"version":    {[]string{"version"}, exitOK},
		"unknown":    {[]string{"inventory"}, exitUsage},
		"typo":       {[]string{"healthchek"}, exitUsage},
		"empty flag": {[]string{"--pull"}, exitUsage},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := dispatch(tc.args); got != tc.want {
				t.Errorf("dispatch(%v) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

// An unrecognised argument must not fall through to serving. Starting a server
// when the operator asked for something else is the wrong kind of surprise for
// a tool that fronts the Docker socket.
func TestDispatchDoesNotServeOnUnknownCommand(t *testing.T) {
	if got := dispatch([]string{"definitely-not-a-command"}); got == exitOK {
		t.Error("an unknown command must not report success")
	}
}
