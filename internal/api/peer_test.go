package api

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ashot0907/ramplus/internal/swap"
)

func swapOpts() swap.Options { return swap.Options{Action: swap.ActionApply, Size: 4 * swap.GiB} }

func TestParseStatusGroups(t *testing.T) {
	tests := []struct {
		file string
		want []uint32
		err  bool
	}{
		{"status", []uint32{4, 24, 27, 30, 46, 100, 990, 991, 1000}, false},
		{"status-no-groups", []uint32{}, false},
		{"status-broken", nil, true},
	}
	for _, tt := range tests {
		f, err := os.Open(filepath.Join("testdata", "proc", tt.file))
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseStatusGroups(f)
		f.Close()
		if (err != nil) != tt.err || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: %v %v, want %v", tt.file, got, err, tt.want)
		}
	}
}

func TestPolicy(t *testing.T) {
	pol := Policy{ReadGID: u32(gidRamplus), AdminGID: u32(gidAdmin)}
	primary := Peer{UID: 1005, GID: gidRamplus} // группа ramplus — основная
	cases := []struct {
		p    Peer
		a    Access
		want bool
	}{
		{peerRoot, AccessAdmin, true},
		{peerReader, AccessRead, true},
		{peerReader, AccessAdmin, false},
		{peerAdmin, AccessAdmin, true},
		{peerAdmin, AccessRead, true},
		{peerStranger, AccessRead, false},
		{peerStranger, AccessAny, true},
		{primary, AccessRead, true},
	}
	for _, c := range cases {
		if got := pol.Allow(c.p, c.a); got != c.want {
			t.Errorf("Allow(%+v, %s) = %v", c.p, c.a, got)
		}
	}
	// Групп нет в системе — только root.
	none := Policy{}
	if none.Allow(peerAdmin, AccessRead) || !none.Allow(peerRoot, AccessAdmin) {
		t.Error("without groups only root is allowed")
	}
}

// Настоящий SO_PEERCRED: свой uid/pid и группы из /proc.
func TestPeerCred(t *testing.T) {
	dir, _ := os.MkdirTemp("", "rpd")
	defer os.RemoveAll(dir)
	l, err := Listen(filepath.Join(dir, "s.sock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := net.Dial("unix", l.Addr().String())
		if err == nil {
			defer c.Close()
			_, _ = c.Read(make([]byte, 1))
		}
	}()
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, err := PeerCred{}.PeerOf(c)
	if err != nil {
		t.Fatal(err)
	}
	if int(p.UID) != os.Getuid() || int(p.GID) != os.Getgid() || int(p.PID) != os.Getpid() {
		t.Errorf("peer %+v, want uid %d gid %d pid %d", p, os.Getuid(), os.Getgid(), os.Getpid())
	}
	groups, _ := os.Getgroups()
	for _, g := range groups {
		if !p.InGroup(uint32(g)) {
			t.Errorf("group %d missing in %+v", g, p.Groups)
		}
	}
}

func TestListen(t *testing.T) {
	dir, _ := os.MkdirTemp("", "rpd")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "run", "d.sock")
	l, err := Listen(sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(sock)
	di, _ := os.Stat(filepath.Dir(sock))
	if fi.Mode().Perm() != SocketMode || fi.Mode()&os.ModeSocket == 0 || di.Mode().Perm() != SocketDirMode {
		t.Errorf("modes: socket %v, dir %v", fi.Mode(), di.Mode())
	}
	// Живой демон — второй не запускается.
	if _, err := Listen(sock, nil); err == nil {
		t.Error("second daemon on a live socket must fail")
	}
	// Оставшийся сокет упавшего демона удаляется.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	l2, err := Listen(sock, nil)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	l2.Close()
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Error("socket must be removed on close")
	}
	// Не сокет — не трогаем.
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(sock, nil); err == nil {
		t.Error("regular file must not be replaced")
	}
}
