package units

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// unit is a stand-in for one systemd unit.
type unit struct {
	load   string // loaded, not-found, masked
	file   string // enabled, disabled, static
	active bool
	broken bool // start "succeeds", then the unit fails
}

// systemd is a stand-in for systemctl: a set of units.
type systemd map[string]*unit

func (s systemd) do(line string, _ []byte) (string, error, bool) {
	f := strings.Fields(line)
	if f[0] != "systemctl" {
		return "", nil, false
	}
	u := s[f[len(f)-1]]
	if u == nil {
		u = &unit{load: "not-found"}
	}
	switch f[1] {
	case "show":
		if f[2] == "-p" && f[3] == "NRestarts" {
			return "0", nil, true
		}
		state := "inactive"
		if u.active {
			state = "active"
		}
		return "LoadState=" + u.load + "\nUnitFileState=" + u.file + "\nActiveState=" + state, nil, true
	case "enable":
		u.file = "enabled"
	case "disable":
		u.file = "disabled"
	case "restart", "start":
		u.active = !u.broken
	case "stop":
		u.active = false
	case "is-failed":
		if u.broken {
			return "", nil, true
		}
		return "", errors.New("exit status 1"), true
	case "is-active":
		if u.active {
			return "", nil, true
		}
		return "", errors.New("exit status 3"), true
	}
	return "", nil, true
}

type world struct {
	t        *testing.T
	checkout string
	sd       systemd
	node     *node.Fake
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T, sd systemd) *world {
	t.Helper()
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, checkout: t.TempDir(), sd: sd, node: &node.Fake{Root: t.TempDir(), Do: sd.do}, rec: rec}
}

func (w *world) list(content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "units")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) tick() engine.Result {
	w.log.Reset()
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{Lane{Node: w.node}}, w.rec, &w.log)
}

func (w *world) ran(prefix string) int {
	n := 0
	for _, c := range w.node.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestAListedUnitIsEnabledStartedAndOwned(t *testing.T) {
	w := newWorld(t, systemd{"systemd-oomd.service": {load: "loaded", file: "disabled"}, "crate-apply.timer": {load: "loaded", file: "disabled"}})
	w.list("# crate\nsystemd-oomd.service\ncrate-apply.timer  # crate apply, every five minutes\n")

	res := w.tick()

	for name, u := range w.sd {
		if u.file != "enabled" || !u.active {
			t.Fatalf("%s: %+v; log:\n%s", name, u, w.log.String())
		}
	}
	if !res.OK() || !reflect.DeepEqual(w.rec.Owned("units"), []string{"crate-apply.timer", "systemd-oomd.service"}) {
		t.Fatalf("ok %v, owned %v", res.OK(), w.rec.Owned("units"))
	}
}

func TestAUnitTheNodeAlreadyRunsIsLeftAloneAndNeverOwned(t *testing.T) {
	w := newWorld(t, systemd{"ssh.service": {load: "loaded", file: "enabled", active: true}})
	w.list("ssh.service\n")
	if res := w.tick(); !res.OK() || len(w.rec.Owned("units")) != 0 || w.ran("systemctl show") != 1 {
		t.Fatalf("ok %v, owned %v, calls %v", res.OK(), w.rec.Owned("units"), w.node.Calls())
	}
	w.list("")
	w.tick()
	if !w.sd["ssh.service"].active {
		t.Fatal("a unit monkeydo never started was stopped when it left git")
	}
}

func TestAUnitMonkeydoStartedIsStoppedAndDisabledWhenItLeavesGit(t *testing.T) {
	w := newWorld(t, systemd{"a.timer": {load: "loaded", file: "disabled"}, "b.timer": {load: "loaded", file: "disabled"}})
	w.list("a.timer\nb.timer\n")
	w.tick()
	w.list("b.timer\n")

	res := w.tick()

	if a := w.sd["a.timer"]; !res.OK() || a.active || a.file != "disabled" || !w.sd["b.timer"].active {
		t.Fatalf("ok %v, a %+v; log:\n%s", res.OK(), a, w.log.String())
	}
	if !reflect.DeepEqual(w.rec.Owned("units"), []string{"b.timer"}) {
		t.Fatalf("owned = %v", w.rec.Owned("units"))
	}
}

func TestAUnitWhoseFileLeftGitHasItsLinksRemoved(t *testing.T) {
	w := newWorld(t, systemd{"a.timer": {load: "loaded", file: "disabled"}})
	w.list("a.timer\n")
	w.tick()
	link := w.node.Path("/etc/systemd/system/timers.target.wants/a.timer")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/systemd/system/a.timer", link); err != nil {
		t.Fatal(err)
	}
	w.sd["a.timer"].load = "not-found" // the files lane removed its file and reloaded; it still runs
	w.list("")

	res := w.tick()

	if _, err := os.Lstat(link); !res.OK() || w.sd["a.timer"].active || !os.IsNotExist(err) {
		t.Fatalf("ok %v, active %v, link %v; log:\n%s", res.OK(), w.sd["a.timer"].active, err, w.log.String())
	}
}

func TestAUnitThatFailsToSettleIsPutBackAndNotOwned(t *testing.T) {
	w := newWorld(t, systemd{"bad.service": {load: "loaded", file: "disabled", broken: true}, "good.service": {load: "loaded", file: "disabled"}})
	w.list("bad.service\ngood.service\n")

	res := w.tick()

	if res.OK() || !reflect.DeepEqual(res.Failed["units"], []string{"bad.service"}) || !strings.Contains(w.log.String(), "bad.service: rolled back") {
		t.Fatalf("failed %v; log:\n%s", res.Failed, w.log.String())
	}
	if bad := w.sd["bad.service"]; bad.file != "disabled" || !w.sd["good.service"].active {
		t.Fatalf("bad %+v, good %+v; want bad disabled again and good running", bad, w.sd["good.service"])
	}
	if !reflect.DeepEqual(w.rec.Owned("units"), []string{"good.service"}) {
		t.Fatalf("owned = %v", w.rec.Owned("units"))
	}
}

func TestAUnitWithNoFileFailsAloneAndNamesWhy(t *testing.T) {
	w := newWorld(t, systemd{"a.timer": {load: "loaded", file: "disabled"}})
	w.list("a.timer\nnope.service\n")
	res := w.tick()
	if res.OK() || !reflect.DeepEqual(res.Failed["units"], []string{"nope.service"}) || !w.sd["a.timer"].active ||
		!strings.Contains(w.log.String(), "not a unit on this node") {
		t.Fatalf("failed %v; log:\n%s", res.Failed, w.log.String())
	}
}

func TestAListWithABadNameIsRefusedWhole(t *testing.T) {
	for _, bad := range []string{"a.timer\n; reboot\n", "dazoo-sync.timer\n", "getty@.service\n", "user.slice\n"} {
		w := newWorld(t, systemd{"a.timer": {load: "loaded", file: "disabled"}})
		w.list(bad)
		if res := w.tick(); res.OK() || len(w.node.Calls()) != 0 {
			t.Fatalf("%q: ok %v, calls %v; want refused and nothing run", bad, res.OK(), w.node.Calls())
		}
	}
}

func TestASecondTickChangesNothing(t *testing.T) {
	w := newWorld(t, systemd{"a.timer": {load: "loaded", file: "disabled"}, "s.service": {load: "loaded", file: "static"}})
	w.list("a.timer\ns.service\n")
	if res := w.tick(); !res.OK() || w.sd["s.service"].file != "static" {
		t.Fatalf("ok %v; a static unit is started, not enabled; log:\n%s", res.OK(), w.log.String())
	}
	before := len(w.node.Calls())
	res := w.tick()
	if !res.OK() || w.log.Len() != 0 || len(w.node.Calls()) != before+2 {
		t.Fatalf("ok %v, log %q, calls %v", res.OK(), w.log.String(), w.node.Calls()[before:])
	}
}
