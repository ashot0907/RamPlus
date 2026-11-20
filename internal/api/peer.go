package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ashot0907/ramplus/internal/detect"
)

// Peer — процесс на другом конце unix-сокета.
type Peer struct {
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	PID    int32    `json:"pid"`
	Groups []uint32 `json:"groups"` // дополнительные группы процесса
}

// InGroup — процесс в группе gid (основной или дополнительной).
func (p Peer) InGroup(gid uint32) bool { return p.GID == gid || slices.Contains(p.Groups, gid) }

// PeerSource определяет, кто подключился. В демоне — SO_PEERCRED, в
// тестах — заданный пользователь (без реальных uid).
type PeerSource interface {
	PeerOf(c net.Conn) (Peer, error)
}

// PeerCred — SO_PEERCRED: uid/gid/pid из ядра (подделать нельзя) плюс
// дополнительные группы процесса из /proc/<pid>/status — ровно те группы,
// с которыми процесс прошёл проверку прав на сокет.
type PeerCred struct {
	ProcRoot string // "" — /proc
}

// PeerOf — учётные данные процесса на другом конце соединения.
func (pc PeerCred) PeerOf(c net.Conn) (Peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return Peer{}, fmt.Errorf("not a unix socket: %T", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Peer{}, err
	}
	if credErr != nil {
		return Peer{}, fmt.Errorf("SO_PEERCRED: %w", credErr)
	}
	p := Peer{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}
	root := pc.ProcRoot
	if root == "" {
		root = "/proc"
	}
	// Нет /proc/<pid> (процесс уже завершился) — только основная группа:
	// это безопасная сторона (меньше прав).
	if f, err := os.Open(filepath.Join(root, strconv.Itoa(int(cred.Pid)), "status")); err == nil {
		groups, err := ParseStatusGroups(f)
		f.Close()
		if err == nil {
			p.Groups = groups
		}
	}
	return p, nil
}

// ParseStatusGroups — дополнительные группы из /proc/<pid>/status (строка
// Groups:). Чистая функция.
func ParseStatusGroups(r io.Reader) ([]uint32, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Groups:")
		if !ok {
			continue
		}
		groups := []uint32{}
		for _, f := range strings.Fields(rest) {
			g, err := strconv.ParseUint(f, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("%w: status: Groups: %q", detect.ErrParse, f)
			}
			groups = append(groups, uint32(g))
		}
		return groups, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: status: no Groups line", detect.ErrParse)
}

// Access — какой доступ нужен эндпоинту.
type Access int

// Уровни доступа.
const (
	AccessAny   Access = iota // любой, кто смог подключиться к сокету (health)
	AccessRead                // status, history, plan: root, ramplus или ramplus-admin
	AccessAdmin               // apply: root или ramplus-admin
)

func (a Access) String() string {
	return [...]string{"any", "read", "admin"}[a]
}

// Policy — кому что можно. nil-группа — такой группы в системе нет:
// доступ только у root.
type Policy struct {
	ReadGID  *uint32 // ramplus
	AdminGID *uint32 // ramplus-admin
}

// Allow — разрешён ли peer доступ a. ramplus-admin включает чтение
// (подключиться к сокету 0660 root:ramplus администратор всё равно может,
// только если он и в ramplus).
func (pol Policy) Allow(p Peer, a Access) bool {
	in := func(g *uint32) bool { return g != nil && p.InGroup(*g) }
	switch {
	case a == AccessAny || p.UID == 0:
		return true
	case a == AccessRead:
		return in(pol.ReadGID) || in(pol.AdminGID)
	case a == AccessAdmin:
		return in(pol.AdminGID)
	}
	return false
}

type peerKey struct{}

type peerInfo struct {
	peer Peer
	err  error
}

func withPeer(ctx context.Context, p Peer, err error) context.Context {
	return context.WithValue(ctx, peerKey{}, peerInfo{p, err})
}

// PeerFrom — кто отправил запрос.
func PeerFrom(ctx context.Context) (Peer, error) {
	pi, ok := ctx.Value(peerKey{}).(peerInfo)
	if !ok {
		return Peer{}, errors.New("unknown client")
	}
	return pi.peer, pi.err
}
