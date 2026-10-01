package collector

import (
	"reflect"
	"testing"
)

func TestDefragTargets(t *testing.T) {
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"btrfs", "filesystem", "defragment", "-r", "/media/raid/a"}, []string{"/media/raid/a"}},
		{[]string{"/usr/bin/btrfs", "fi", "defrag", "-czstd", "-t", "32M", "x", "y"}, []string{"x", "y"}},
		{[]string{"btrfs", "-v", "fi", "de", "-l", "1G", "-s", "0", "/srv"}, []string{"/srv"}},
		{[]string{"btrfs", "fi", "defrag", "--", "-weird"}, []string{"-weird"}},
		{[]string{"btrfs", "filesystem", "df", "/"}, nil},
		{[]string{"btrfs", "f", "defrag", "/"}, nil},
		{[]string{"grep", "defragment", "/"}, nil},
	}
	for _, c := range cases {
		if got := defragTargets(c.args); !reflect.DeepEqual(got, c.want) {
			t.Errorf("defragTargets(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestUUIDForPath(t *testing.T) {
	mounts := map[string]string{"/": "root", "/media/raid": "raid", "/media/raid/sub": "raid"}
	cases := map[string]string{
		"/":                  "root",
		"/home/x":            "root",
		"/media/raid":        "raid",
		"/media/raid/a/b":    "raid",
		"/media/raidx/a":     "root",
		"/media/raid/sub/zz": "raid",
	}
	for path, want := range cases {
		if got := uuidForPath(path, mounts); got != want {
			t.Errorf("uuidForPath(%q) = %q, want %q", path, got, want)
		}
	}
}
