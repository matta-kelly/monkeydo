package accounts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// host is a stand-in for the account tools: users and linger in memory, ranges in real files under the node's root.
type host struct {
	t        *testing.T
	n        *node.Fake
	users    map[string]bool
	linger   map[string]bool
	autoSub  bool // useradd gives the account a range itself
	noLinger bool // enable-linger "succeeds" but does nothing
}

func (h *host) appendRange(file, name string, start int) {
	p := h.n.Path(file)
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		h.t.Fatal(err)
	}
	fmt.Fprintf(f, "%s:%d:%d\n", name, start, rangeSize)
	f.Close()
}

func (h *host) do(line string, _ []byte) (string, error, bool) {
	f := strings.Fields(line)
	switch {
	case f[0] == "getent":
		if h.users[f[2]] {
			return f[2] + ":x:1001:1001::/home/" + f[2] + ":/usr/sbin/nologin", nil, true
		}
		return "", errors.New("exit status 2"), true
	case f[0] == "useradd":
		a := f[len(f)-1]
		h.users[a] = true
		if h.autoSub {
			h.appendRange("/etc/subuid", a, 900000)
			h.appendRange("/etc/subgid", a, 900000)
		}
		return "", nil, true
	case f[0] == "usermod":
		var start int
		fmt.Sscanf(f[2], "%d-", &start)
		file := "/etc/subuid"
		if f[1] == "--add-subgids" {
			file = "/etc/subgid"
		}
		h.appendRange(file, f[3], start)
		return "", nil, true
	case f[0] == "userdel":
		delete(h.users, f[1])
		return "", nil, true
	case f[0] == "loginctl" && f[1] == "show-user":
		if h.linger[f[2]] {
			return "yes", nil, true
		}
		return "no", nil, true
	case f[0] == "loginctl" && f[1] == "enable-linger":
		if !h.noLinger {
			h.linger[f[2]] = true
		}
		return "", nil, true
	}
	return "", nil, false
}

type world struct {
	t        *testing.T
	checkout string
	host     *host
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	n := &node.Fake{Root: t.TempDir()}
	h := &host{t: t, n: n, users: map[string]bool{}, linger: map[string]bool{}}
	n.Do = h.do
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.appendRange("/etc/subuid", "core", 100000)
	h.appendRange("/etc/subgid", "core", 100000)
	h.users["core"] = true
	return &world{t: t, checkout: t.TempDir(), host: h, rec: rec}
}

func (w *world) list(content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "accounts")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) tick() engine.Result {
	w.log.Reset()
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{Lane{Node: w.host.n}}, w.rec, &w.log)
}

func (w *world) file(p string) string {
	data, _ := os.ReadFile(w.host.n.Path(p))
	return string(data)
}

func TestANewAccountGetsAHomeItsOwnRangesAndLinger(t *testing.T) {
	w := newWorld(t)
	w.list("# landl-db's account\nlandl-db\n")

	res := w.tick()

	if !res.OK() || !w.host.users["landl-db"] || !w.host.linger["landl-db"] {
		t.Fatalf("ok %v, users %v, linger %v; log:\n%s", res.OK(), w.host.users, w.host.linger, w.log.String())
	}
	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if !strings.Contains(w.file(f), "landl-db:165536:65536") {
			t.Fatalf("%s = %q; want a range past core's", f, w.file(f))
		}
	}
	if !reflect.DeepEqual(w.rec.Owned("accounts"), []string{"landl-db"}) {
		t.Fatalf("owned = %v", w.rec.Owned("accounts"))
	}
}

func TestARangeUseraddGaveIsKeptAndNoSecondOneAdded(t *testing.T) {
	w := newWorld(t)
	w.host.autoSub = true
	w.list("landl-dash\n")
	w.tick()
	if got := strings.Count(w.file("/etc/subuid"), "landl-dash:"); got != 1 {
		t.Fatalf("landl-dash has %d subuid lines: %q", got, w.file("/etc/subuid"))
	}
}

func TestAnAccountThatExistsButLacksLingerOnlyGetsLinger(t *testing.T) {
	w := newWorld(t)
	w.list("core\n")
	w.tick()
	for _, c := range w.host.n.Calls() {
		if strings.HasPrefix(c, "useradd") || strings.HasPrefix(c, "usermod") {
			t.Fatalf("an account with a home and ranges was changed: %s", c)
		}
	}
	if !w.host.linger["core"] {
		t.Fatal("core does not linger")
	}
}

func TestAnAccountThatLeftGitIsLeftInPlace(t *testing.T) {
	w := newWorld(t)
	w.list("landl-db\n")
	w.tick()
	w.list("")

	res := w.tick()

	if !res.OK() || !w.host.users["landl-db"] || len(w.rec.Owned("accounts")) != 0 {
		t.Fatalf("ok %v, still there %v, owned %v; want kept on the node and no longer owned", res.OK(), w.host.users["landl-db"], w.rec.Owned("accounts"))
	}
	for _, c := range w.host.n.Calls() {
		if strings.HasPrefix(c, "userdel") {
			t.Fatal("an account that left git was deleted")
		}
	}
}

func TestANewAccountThatDoesNotTakeIsRemovedAgain(t *testing.T) {
	w := newWorld(t)
	w.host.noLinger = true
	w.list("landl-db\n")

	res := w.tick()

	if res.OK() || w.host.users["landl-db"] || len(w.rec.Owned("accounts")) != 0 {
		t.Fatalf("ok %v, user left %v, owned %v; log:\n%s", res.OK(), w.host.users["landl-db"], w.rec.Owned("accounts"), w.log.String())
	}
}

func TestRootAndBadNamesAreRefused(t *testing.T) {
	for _, bad := range []string{"root", "Bad Name", "a;rm"} {
		w := newWorld(t)
		w.list("landl-db\n" + bad + "\n")
		if res := w.tick(); res.OK() || w.host.users["landl-db"] {
			t.Fatalf("%q: ok %v, created %v; want refused whole", bad, res.OK(), w.host.users["landl-db"])
		}
	}
}

func TestASecondTickChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.list("landl-db\n")
	w.tick()
	n := len(w.host.n.Calls())
	res := w.tick()
	for _, c := range w.host.n.Calls()[n:] {
		if !strings.HasPrefix(c, "getent") && !strings.HasPrefix(c, "loginctl show-user") {
			t.Fatalf("a second tick changed something: %s", c)
		}
	}
	if !res.OK() || w.log.Len() != 0 {
		t.Fatalf("ok %v, log %q", res.OK(), w.log.String())
	}
}

func TestTheContainersRangeIsWrittenWithNoUser(t *testing.T) {
	w := newWorld(t)
	w.list("core\ncontainers\n")

	res := w.tick()

	for _, f := range []string{"/etc/subuid", "/etc/subgid"} {
		if !strings.Contains(w.file(f), "containers:2147483647:2147483648\n") || !strings.Contains(w.file(f), "core:100000:65536") {
			t.Fatalf("ok %v, %s = %q; log:\n%s", res.OK(), f, w.file(f), w.log.String())
		}
	}
	for _, c := range w.host.n.Calls() {
		if strings.Contains(c, "useradd") || strings.Contains(c, "linger containers") || strings.Contains(c, "show-user containers") {
			t.Fatalf("the range made or asked for a user: %s", c)
		}
	}
	if !res.OK() {
		t.Fatalf("log:\n%s", w.log.String())
	}
	if res := w.tick(); !res.OK() || w.log.Len() != 0 {
		t.Fatalf("a second tick: ok %v, log %q", res.OK(), w.log.String())
	}
}

func TestANewAccountsRangeGoesBelowTheContainersRange(t *testing.T) {
	w := newWorld(t)
	w.list("containers\n")
	w.tick()
	w.list("containers\nshop\n")

	res := w.tick()

	if !res.OK() || !strings.Contains(w.file("/etc/subuid"), "shop:165536:65536") {
		t.Fatalf("ok %v, subuid %q; log:\n%s", res.OK(), w.file("/etc/subuid"), w.log.String())
	}
}

func TestTheContainersRangeIsRefusedWhereItOverlaps(t *testing.T) {
	w := newWorld(t)
	w.host.appendRange("/etc/subuid", "big", 2147483000)
	w.list("containers\n")
	if res := w.tick(); res.OK() || strings.Contains(w.file("/etc/subuid"), "containers:") {
		t.Fatalf("ok %v, subuid %q", res.OK(), w.file("/etc/subuid"))
	}
}
