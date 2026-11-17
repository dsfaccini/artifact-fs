package mounts

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseMountinfoReadsEachMount(t *testing.T) {
	t.Parallel()
	text := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
30 22 0:50 / /home/u/wt\040x rw,nosuid,nodev,relatime shared:2 master:1 - fuse.artifact-fs artifact-fs:repo rw,user_id=1000
31 30 0:51 / /home/u/wt\040x rw - fuse.artifact-fs artifact-fs:back\134slash rw
`
	entries, err := parseMountinfo(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []mountinfoEntry{
		{id: 22, major: 8, minor: 1, point: "/", fstype: "ext4", source: "/dev/sda1"},
		{id: 30, major: 0, minor: 50, point: "/home/u/wt x", fstype: "fuse.artifact-fs", source: "artifact-fs:repo"},
		{id: 31, major: 0, minor: 51, point: "/home/u/wt x", fstype: "fuse.artifact-fs", source: `artifact-fs:back\slash`},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("parseMountinfo = %+v\nwant %+v", entries, want)
	}
	// The later mount at a point is on top.
	if e, ok := mountAt(entries, "/home/u/wt x"); !ok || e.id != 31 {
		t.Fatalf("mountAt = %+v, %v; want mount 31", e, ok)
	}
	if e, ok := mountAt(entries, "/home/u/wt"); ok {
		t.Fatalf("mountAt a path with no mount = %+v", e)
	}
	if _, err := parseMountinfo("22 1 8:1 / / rw shared:1 ext4 /dev/sda1 rw\n"); err == nil {
		t.Fatal("parseMountinfo accepted a line without the - separator")
	}
}

func TestHoldingMountIsTheDeepestAndOfThoseStackedThereTheLast(t *testing.T) {
	t.Parallel()
	path := "/tmp/x/work"
	// An ancestor listed after the deeper mounts must not win.
	mounts := []struct{ dev, point string }{
		{"0:10", "/"}, {"0:20", "/tmp/x"}, {"0:30", path + "-sibling"}, {"0:40", path}, {"0:50", path},
		{"0:60", path + "/below"}, {"0:70", "/tmp/x"},
	}
	var lines []string
	for i, m := range mounts {
		lines = append(lines, fmt.Sprintf("%d 1 %s / %s rw - ext4 /dev/x rw", i, m.dev, m.point))
	}
	entries, err := parseMountinfo(strings.Join(lines, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "/file"} {
		if e, ok := holdingMount(entries, p); !ok || e.major != 0 || e.minor != 50 {
			t.Fatalf("holdingMount(%s) = %+v, %v; want 0:50", p, e, ok)
		}
	}
}

func TestStatFieldsStartAfterTheCommandsLastParenthesis(t *testing.T) {
	t.Parallel()
	fields := []string{"S", "77"}
	for i := 5; i <= 52; i++ {
		fields = append(fields, fmt.Sprint(i))
	}
	fields[19] = "123456"
	got, err := statFields("4242 (a) b (c)) " + strings.Join(fields, " ") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != "S" || got[1] != "77" || got[19] != "123456" {
		t.Fatalf("state, parent, start time = %s, %s, %s; want S, 77, 123456", got[0], got[1], got[19])
	}
}

func TestParseLsofNamesEachProcessOnTheDeviceUnderThePath(t *testing.T) {
	t.Parallel()
	out := strings.Join([]string{
		"p1", "R0", "claunchd", "fcwd", "D0x1000012", "n/",
		"p100", "R1", "czsh", "fcwd", "D0x1000012", "n/Users/u/wt",
		"p101", "R100", "csleep", "fcwd", "D0x1000012", "n/Users/u", "f3", "D0x1000012", "n/Users/u/wt/notes.txt",
		// lsof prints the device unsigned; only its low 32 bits count.
		"p102", "R100", "cmapper", "ftxt", "D0x700001000012", "n/Users/u/wt/lib.dylib",
		// The directory beneath the mount: another device.
		"p200", "R1", "cbeneath", "fcwd", "D0x2000001", "n/Users/u/wt",
		"p300", "R1", "csibling", "fcwd", "D0x1000012", "n/Users/u/wt-sibling",
		// A file set without a device.
		"p400", "R1", "cnodev", "fcwd", "n/Users/u/wt",
		"",
	}, "\n")
	got := parseLsof(out, "/Users/u/wt", 0x1000012, 0)
	want := []Holder{
		{PID: 100, Command: "zsh", ParentPID: 1, ParentCommand: "launchd"},
		{PID: 101, Command: "sleep", ParentPID: 100, ParentCommand: "zsh"},
		{PID: 102, Command: "mapper", ParentPID: 100, ParentCommand: "zsh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseLsof = %+v\nwant %+v", got, want)
	}
	if got := parseLsof(out, "/Users/u/wt", 0x1000012, 100); len(got) != 0 {
		t.Fatalf("parseLsof ignoring 100 = %+v; want none: the others descend from it", got)
	}
}
