package main

import "testing"

const mountinfo = `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
35 22 0:31 / /sys/fs/fuse/connections rw,relatime shared:12 - fusectl fusectl rw
40 22 0:52 / /mnt/f rw,nosuid,nodev,relatime shared:20 - fuse.mountable mountable rw,user_id=0,group_id=0
41 22 0:53 / /mnt/with\040space rw,nosuid,nodev shared:21 - fuse.mountable mountable rw,user_id=0
42 22 8:2 / /mnt/disk rw shared:22 - ext4 /dev/sda2 rw
43 40 0:60 / /mnt/f rw,nosuid,nodev shared:23 opt:1 - fuse.mountable mountable rw,user_id=0
`

func TestFuseConnection(t *testing.T) {
	cases := []struct {
		dir, id string
		ok      bool
	}{
		{"/mnt/f", "60", true}, // the mount on top
		{"/mnt/f/", "60", true},
		{"/mnt/with space", "53", true},
		{"/mnt/disk", "", false},
		{"/mnt/none", "", false},
	}
	for _, c := range cases {
		id, ok := fuseConnection([]byte(mountinfo), c.dir)
		if id != c.id || ok != c.ok {
			t.Errorf("fuseConnection(%q) = %q, %v; want %q, %v", c.dir, id, ok, c.id, c.ok)
		}
	}
}

func TestFuseConnectionUsesTheKernelDeviceNumber(t *testing.T) {
	id, ok := fuseConnection([]byte("50 22 1:2 / /m rw - fuse /dev/fuse rw\n"), "/m")
	if !ok || id != "1048578" { // MKDEV(1, 2)
		t.Fatalf("got %q, %v", id, ok)
	}
}
