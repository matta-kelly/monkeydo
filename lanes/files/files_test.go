package files

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

const writeback = "vm.dirty_background_bytes = 268435456\nvm.dirty_bytes = 1073741824\n"

// world is a dazoo checkout and a node, each in its own temp folder.
type world struct {
	t        *testing.T
	checkout string
	node     *node.Fake
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, checkout: t.TempDir(), node: &node.Fake{Root: t.TempDir(), Answers: map[string]string{
		"sysctl -n vm.dirty_background_bytes": "268435456", "sysctl -n vm.dirty_bytes": "1073741824",
	}, Fails: map[string]string{}}, rec: rec}
}

// git puts a file in a tree: shared ("_shared") or the node's ("mutable/n").
func (w *world) git(tree, rel, content string, mode os.FileMode) {
	p := filepath.Join(w.checkout, "nodes", tree, "etc", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) ungit(tree, rel string) {
	os.Remove(filepath.Join(w.checkout, "nodes", tree, "etc", rel))
}

// box puts a file on the node.
func (w *world) box(path, content string, mode os.FileMode) {
	if err := node.Write(w.node, path, node.File{Data: []byte(content), Mode: mode}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) tick() engine.Result {
	w.log.Reset()
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{Lane{Node: w.node, Window: units.Window{For: 2 * time.Nanosecond, Step: time.Nanosecond}}}, w.rec, &w.log)
}

func (w *world) onBox(path string) (string, os.FileMode, bool) {
	f, ok, err := node.Read(w.node, path)
	if err != nil {
		w.t.Fatal(err)
	}
	return string(f.Data), f.Mode, ok
}

func TestANewSysctlFileIsWrittenAppliedAndReadBack(t *testing.T) {
	w := newWorld(t)
	w.git("_shared", "sysctl.d/80-writeback.conf", writeback, 0o644)

	res := w.tick()

	data, mode, ok := w.onBox("/etc/sysctl.d/80-writeback.conf")
	if !res.OK() || !ok || data != writeback || mode != 0o644 {
		t.Fatalf("ok %v, on box %v %q %o; log:\n%s", res.OK(), ok, data, mode, w.log.String())
	}
	want := []string{"sysctl --system", "sysctl -n vm.dirty_background_bytes", "sysctl -n vm.dirty_bytes"}
	if !reflect.DeepEqual(w.node.Calls(), want) {
		t.Fatalf("calls = %v, want %v", w.node.Calls(), want)
	}
	if !reflect.DeepEqual(w.rec.Owned("files"), []string{"/etc/sysctl.d/80-writeback.conf"}) {
		t.Fatalf("owned = %v", w.rec.Owned("files"))
	}
}

func TestASysctlValueThatDoesNotReadBackIsRolledBack(t *testing.T) {
	w := newWorld(t)
	w.node.Answers["sysctl -n vm.dirty_bytes"] = "0"
	w.git("_shared", "sysctl.d/80-writeback.conf", writeback, 0o644)

	res := w.tick()

	if _, _, ok := w.onBox("/etc/sysctl.d/80-writeback.conf"); ok || res.OK() {
		t.Fatalf("the file is still on the box (%v), or the tick says ok (%v)", ok, res.OK())
	}
	if calls := w.node.Calls(); calls[len(calls)-1] != "sysctl --system" {
		t.Fatalf("the rollback did not apply the old values again: %v", calls)
	}
	if len(w.rec.Owned("files")) != 0 || !strings.Contains(w.log.String(), "rolled back") {
		t.Fatalf("owned %v; log:\n%s", w.rec.Owned("files"), w.log.String())
	}
}

func TestAFileTheBoxAlreadyHasIsAdoptedWithNoReload(t *testing.T) {
	w := newWorld(t)
	w.git("_shared", "sysctl.d/80-writeback.conf", writeback, 0o644)
	w.box("/etc/sysctl.d/80-writeback.conf", writeback, 0o644)

	res := w.tick()

	if !res.OK() || len(w.node.Calls()) != 0 || w.log.Len() != 0 {
		t.Fatalf("ok %v, calls %v, log %q; want nothing done", res.OK(), w.node.Calls(), w.log.String())
	}
	if !reflect.DeepEqual(w.rec.Owned("files"), []string{"/etc/sysctl.d/80-writeback.conf"}) {
		t.Fatalf("owned = %v; the file should be adopted", w.rec.Owned("files"))
	}
}

func TestTheNodesTreeWinsOverTheSharedOne(t *testing.T) {
	w := newWorld(t)
	w.git("_shared", "dazoo/x.conf", "shared\n", 0o644)
	w.git("mutable/n", "dazoo/x.conf", "node\n", 0o644)
	w.tick()
	if data, _, _ := w.onBox("/etc/dazoo/x.conf"); data != "node\n" {
		t.Fatalf("on box %q, want the node's", data)
	}
}

func TestAFileThatLeftGitIsRemovedAndOneMonkeydoNeverOwnedIsNot(t *testing.T) {
	w := newWorld(t)
	w.git("_shared", "dazoo/keep.conf", "keep\n", 0o644)
	w.git("_shared", "dazoo/drop.conf", "drop\n", 0o644)
	w.tick()
	w.box("/etc/dazoo/someone-elses.conf", "theirs\n", 0o644)
	w.ungit("_shared", "dazoo/drop.conf")

	res := w.tick()

	if _, _, ok := w.onBox("/etc/dazoo/drop.conf"); ok || !res.OK() {
		t.Fatalf("drop.conf still on the box (%v), or not ok (%v): %s", ok, res.OK(), w.log.String())
	}
	if _, _, ok := w.onBox("/etc/dazoo/someone-elses.conf"); !ok {
		t.Fatal("a file monkeydo never owned was removed")
	}
	if !reflect.DeepEqual(w.rec.Owned("files"), []string{"/etc/dazoo/keep.conf"}) {
		t.Fatalf("owned = %v", w.rec.Owned("files"))
	}
}

func TestATreeNamingAFileMonkeydoMayNotOwnIsRefusedWhole(t *testing.T) {
	for _, rel := range []string{"dazoo/sync.env", "containers/systemd/web.container", "dazoo/age.key", "ssh/ssh_host_ed25519_key"} {
		w := newWorld(t)
		w.git("_shared", "sysctl.d/80-writeback.conf", writeback, 0o644)
		w.git("mutable/n", rel, "x\n", 0o644)
		res := w.tick()
		if res.OK() || !strings.Contains(w.log.String(), "not monkeydo's") {
			t.Fatalf("%s: ok %v, log:\n%s", rel, res.OK(), w.log.String())
		}
		if _, _, ok := w.onBox("/etc/sysctl.d/80-writeback.conf"); ok {
			t.Fatalf("%s: a refused tree still wrote a file", rel)
		}
	}
}

func TestAnEmptyTreeDoesNotRemoveEverything(t *testing.T) {
	w := newWorld(t)
	w.git("_shared", "dazoo/a.conf", "a\n", 0o644)
	w.tick()
	w.ungit("_shared", "dazoo/a.conf")

	res := w.tick()

	if _, _, ok := w.onBox("/etc/dazoo/a.conf"); !ok || res.OK() {
		t.Fatalf("on box %v, ok %v; want kept and refused", ok, res.OK())
	}
}

func TestAnSshdFileSshdRefusesIsPutBack(t *testing.T) {
	w := newWorld(t)
	w.box("/etc/ssh/sshd_config.d/50-hardening.conf", "PermitRootLogin no\n", 0o644)
	w.git("_shared", "ssh/sshd_config.d/50-hardening.conf", "Bogus yes\n", 0o644)
	w.node.Fails["sshd -t"] = "Bad configuration option: Bogus"

	res := w.tick()

	if data, _, _ := w.onBox("/etc/ssh/sshd_config.d/50-hardening.conf"); data != "PermitRootLogin no\n" || res.OK() {
		t.Fatalf("on box %q, ok %v; want the old file back", data, res.OK())
	}
	for _, c := range w.node.Calls() {
		if c == "systemctl reload ssh" {
			t.Fatal("ssh was reloaded on a config sshd refused")
		}
	}
}

func TestAnExecutableFileIs0755AndModeDriftIsFixed(t *testing.T) {
	w := newWorld(t)
	w.git("mutable/n", "dazoo/media/restore.sh", "#!/bin/sh\n", 0o755)
	w.git("mutable/n", "dazoo/a.conf", "a\n", 0o644)
	w.box("/etc/dazoo/a.conf", "a\n", 0o600)

	w.tick()

	if _, mode, _ := w.onBox("/etc/dazoo/media/restore.sh"); mode != 0o755 {
		t.Fatalf("restore.sh mode %o", mode)
	}
	if _, mode, _ := w.onBox("/etc/dazoo/a.conf"); mode != 0o644 {
		t.Fatalf("a.conf mode %o, want the drift fixed", mode)
	}
}

func TestANewUnitFileIsWrittenAndReloadedButNotStarted(t *testing.T) {
	w := newWorld(t)
	w.node.Fails["systemctl is-active --quiet crate-apply.timer"] = "inactive"
	w.git("mutable/n", "systemd/system/crate-apply.timer", "[Timer]\nOnCalendar=*:0/5\n", 0o644)

	res := w.tick()

	want := []string{"systemctl daemon-reload", "systemctl is-active --quiet crate-apply.timer"}
	if !res.OK() || !reflect.DeepEqual(w.node.Calls(), want) {
		t.Fatalf("ok %v, calls %v; log:\n%s", res.OK(), w.node.Calls(), w.log.String())
	}
}

func TestAChangedFileOfARunningUnitRestartsIt(t *testing.T) {
	w := newWorld(t)
	w.box("/etc/systemd/system/web.service.d/limits.conf", "[Service]\nMemoryMax=1G\n", 0o644)
	w.git("mutable/n", "systemd/system/web.service.d/limits.conf", "[Service]\nMemoryMax=2G\n", 0o644)
	w.node.Fails["systemctl is-failed --quiet web.service"] = "active"

	res := w.tick()

	if !res.OK() || !contains(w.node.Calls(), "systemctl restart web.service") {
		t.Fatalf("ok %v, calls %v; log:\n%s", res.OK(), w.node.Calls(), w.log.String())
	}
}

func TestAUnitThatFailsAfterItsChangeGetsItsOldFileBack(t *testing.T) {
	w := newWorld(t)
	old := "[Service]\nExecStart=/bin/true\n"
	w.box("/etc/systemd/system/web.service", old, 0o644)
	w.git("mutable/n", "systemd/system/web.service", "[Service]\nExecStart=/bin/false\n", 0o644)
	// running before the change; failed while the new file is on the node
	w.node.Do = func(line string, _ []byte) (string, error, bool) {
		if line != "systemctl is-failed --quiet web.service" {
			return "", nil, false
		}
		if data, _, _ := w.onBox("/etc/systemd/system/web.service"); data == old {
			return "", errors.New("exit status 1"), true
		}
		return "", nil, true
	}

	res := w.tick()

	data, _, _ := w.onBox("/etc/systemd/system/web.service")
	if res.OK() || data != old || !strings.Contains(w.log.String(), "web.service: rolled back") {
		t.Fatalf("ok %v, on box %q; log:\n%s", res.OK(), data, w.log.String())
	}
}

func TestATemplateOrSliceFileIsOnlyReloaded(t *testing.T) {
	w := newWorld(t)
	w.box("/etc/systemd/system/user@.service.d/limits.conf", "old\n", 0o644)
	w.box("/etc/systemd/system/user-1000.slice.d/weight.conf", "old\n", 0o644)
	w.git("mutable/n", "systemd/system/user@.service.d/limits.conf", "[Service]\nTasksMax=4096\n", 0o644)
	w.git("mutable/n", "systemd/system/user-1000.slice.d/weight.conf", "[Slice]\nCPUWeight=25\n", 0o644)

	res := w.tick()

	want := []string{"systemctl daemon-reload", "systemctl daemon-reload"}
	if !res.OK() || !reflect.DeepEqual(w.node.Calls(), want) {
		t.Fatalf("ok %v, calls %v; want reloads only, never a restart of user@", res.OK(), w.node.Calls())
	}
}

func TestRestartableNamesTheUnitAFileDefines(t *testing.T) {
	for path, want := range map[string]string{
		"/etc/systemd/system/crate-apply.timer":                 "crate-apply.timer",
		"/etc/systemd/system/web.service.d/limits.conf":         "web.service",
		"/etc/systemd/system/user@.service.d/delegate.conf":     "",
		"/etc/systemd/system/user.slice.d/crate.conf":           "",
		"/etc/systemd/system/timers.target.wants/a.timer":       "",
		"/etc/systemd/system/systemd-oomd.service.d/x.conf":     "systemd-oomd.service",
		"/etc/systemd/system/multi-user.target.wants/a.service": "",
	} {
		if got := restartable(path); got != want {
			t.Errorf("restartable(%s) = %q, want %q", path, got, want)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
