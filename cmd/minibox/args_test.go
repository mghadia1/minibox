package main

import (
	"slices"
	"testing"

	"github.com/mghadia1/minibox/internal/image"
)

func TestImageArgs(t *testing.T) {
	var withBoth, cmdOnly image.Config
	withBoth.Config.Entrypoint, withBoth.Config.Cmd = []string{"/entry", "-x"}, []string{"default"}
	cmdOnly.Config.Cmd = []string{"/bin/sh"}
	for _, tc := range []struct {
		cfg     image.Config
		ep      optionalString
		command []string
		want    []string
	}{
		{withBoth, optionalString{}, nil, []string{"/entry", "-x", "default"}},
		{withBoth, optionalString{}, []string{"given"}, []string{"/entry", "-x", "given"}},
		{withBoth, optionalString{"/other", true}, nil, []string{"/other"}},
		{withBoth, optionalString{"/other", true}, []string{"a"}, []string{"/other", "a"}},
		{withBoth, optionalString{"", true}, []string{"ls"}, []string{"ls"}},
		{withBoth, optionalString{"", true}, nil, []string{}},
		{cmdOnly, optionalString{}, nil, []string{"/bin/sh"}},
		{cmdOnly, optionalString{}, []string{"echo", "hi"}, []string{"echo", "hi"}},
	} {
		if got := imageArgs(tc.cfg, tc.ep, tc.command); !slices.Equal(got, tc.want) {
			t.Errorf("imageArgs(%+v, %+v, %q) = %q, want %q", tc.cfg.Config, tc.ep, tc.command, got, tc.want)
		}
	}
}
