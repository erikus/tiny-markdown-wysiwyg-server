package main

import (
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
		check   func(t *testing.T, o *options)
	}{
		{
			name: "minimal",
			args: []string{"--root", "notes=/tmp/notes"},
			check: func(t *testing.T, o *options) {
				if len(o.roots) != 1 || o.roots[0].Name != "notes" || o.roots[0].Dir != "/tmp/notes" {
					t.Fatalf("roots %v", o.roots)
				}
				if o.listen != defaultListen || o.ext != ".md,.markdown" {
					t.Fatalf("defaults %+v", o)
				}
			},
		},
		{
			name: "repeatable flags",
			args: []string{"--root", "a=/a", "--root", "b=/b", "--allowed-host", "x", "--allowed-host", "y", "--tsnet-hostname", "edit"},
			check: func(t *testing.T, o *options) {
				if len(o.roots) != 2 || len(o.allowedHosts) != 2 || o.tsnetHostname != "edit" {
					t.Fatalf("%+v", o)
				}
			},
		},
		{name: "no roots", args: nil, wantErr: "--root"},
		{name: "root without dir", args: []string{"--root", "notes"}, wantErr: "NAME=DIR"},
		{name: "root empty name", args: []string{"--root", "=/x"}, wantErr: "NAME=DIR"},
		{name: "bad listen", args: []string{"--root", "a=/a", "--listen", "nope"}, wantErr: "--listen"},
		{name: "zero max bytes", args: []string{"--root", "a=/a", "--max-bytes", "0"}, wantErr: "positive"},
		{name: "positional", args: []string{"--root", "a=/a", "extra"}, wantErr: "unexpected"},
		{name: "unknown flag", args: []string{"--root", "a=/a", "--bogus"}, wantErr: "bogus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseFlags(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, o)
		})
	}
}

func TestDefaultTsnetDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg")
	if got := defaultTsnetDir(); got != "/xdg/mdedit/tsnet" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := defaultTsnetDir(); got != "/home/u/.local/state/mdedit/tsnet" {
		t.Fatalf("got %q", got)
	}
}
