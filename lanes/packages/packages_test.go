package packages

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

// apt is a stand-in for dpkg and apt-get: a set of installed packages, and packages that fail to install.
type apt struct {
	installed map[string]bool
	broken    map[string]bool // install "succeeds" but leaves it not installed
	missing   map[string]bool // apt-get install fails: no such package
}

func (a *apt) do(line string, _ []byte) (string, error, bool) {
	f := strings.Fields(line)
	switch {
	case f[0] == "dpkg-query":
		p := f[len(f)-1]
		if a.installed[p] {
			return "install ok installed", nil, true
		}
		return "", errors.New("no packages found matching " + p), true
	case f[0] == "apt-get" && f[1] == "update":
		return "", nil, true
	case f[0] == "apt-get" && f[1] == "install":
		p := f[len(f)-1]
		if a.missing[p] {
			return "E: Unable to locate package " + p, errors.New("exit status 100"), true
		}
		if !a.broken[p] {
			a.installed[p] = true
		}
		return "", nil, true
	case f[0] == "apt-get" && f[1] == "remove":
		delete(a.installed, f[len(f)-1])
		return "", nil, true
	}
	return "", nil, false
}

type world struct {
	t        *testing.T
	checkout string
	apt      *apt
	node     *node.Fake
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T, installed ...string) *world {
	t.Helper()
	a := &apt{installed: map[string]bool{}, broken: map[string]bool{}, missing: map[string]bool{}}
	for _, p := range installed {
		a.installed[p] = true
	}
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, checkout: t.TempDir(), apt: a, node: &node.Fake{Root: t.TempDir(), Do: a.do}, rec: rec}
}

func (w *world) list(content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "packages")
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

func TestMissingPackagesAreInstalledAfterOneUpdateAndOwned(t *testing.T) {
	w := newWorld(t)
	w.list("# the host contract\ncatatonit\npasst  # rootless networking\nuidmap\n")

	res := w.tick()

	for _, p := range []string{"catatonit", "passt", "uidmap"} {
		if !w.apt.installed[p] {
			t.Fatalf("%s not installed; log:\n%s", p, w.log.String())
		}
	}
	if !res.OK() || w.ran("apt-get update") != 1 {
		t.Fatalf("ok %v, updates %d; want ok and one update", res.OK(), w.ran("apt-get update"))
	}
	if !reflect.DeepEqual(w.rec.Owned("packages"), []string{"catatonit", "passt", "uidmap"}) {
		t.Fatalf("owned = %v", w.rec.Owned("packages"))
	}
}

func TestAPackageTheNodeAlreadyHasIsLeftAloneAndNeverOwned(t *testing.T) {
	w := newWorld(t, "curl")
	w.list("curl\n")

	res := w.tick()
	if !res.OK() || w.ran("apt-get") != 0 || len(w.rec.Owned("packages")) != 0 {
		t.Fatalf("ok %v, apt calls %d, owned %v; want nothing done and nothing owned", res.OK(), w.ran("apt-get"), w.rec.Owned("packages"))
	}

	w.list("")
	w.tick()
	if !w.apt.installed["curl"] {
		t.Fatal("a package monkeydo never installed was removed when it left git")
	}
}

func TestAPackageMonkeydoInstalledIsRemovedWhenItLeavesGit(t *testing.T) {
	w := newWorld(t)
	w.list("catatonit\npasst\n")
	w.tick()
	w.list("passt\n")

	res := w.tick()

	if !res.OK() || w.apt.installed["catatonit"] || !w.apt.installed["passt"] {
		t.Fatalf("ok %v, installed %v; log:\n%s", res.OK(), w.apt.installed, w.log.String())
	}
	if !reflect.DeepEqual(w.rec.Owned("packages"), []string{"passt"}) {
		t.Fatalf("owned = %v", w.rec.Owned("packages"))
	}
}

func TestAPackageAptCannotFindFailsAloneAndTheRestInstall(t *testing.T) {
	w := newWorld(t)
	w.apt.missing["nosuchpkg"] = true
	w.list("catatonit\nnosuchpkg\nuidmap\n")

	res := w.tick()

	if res.OK() || !reflect.DeepEqual(res.Failed["packages"], []string{"nosuchpkg"}) {
		t.Fatalf("failed = %v", res.Failed)
	}
	if !w.apt.installed["catatonit"] || !w.apt.installed["uidmap"] {
		t.Fatalf("the other packages were not installed: %v", w.apt.installed)
	}
	if !reflect.DeepEqual(w.rec.Owned("packages"), []string{"catatonit", "uidmap"}) {
		t.Fatalf("owned = %v; a failed install is not owned", w.rec.Owned("packages"))
	}
}

func TestAnInstallThatLeavesThePackageMissingIsUnhealthy(t *testing.T) {
	w := newWorld(t)
	w.apt.broken["catatonit"] = true
	w.list("catatonit\n")
	if res := w.tick(); res.OK() || !strings.Contains(w.log.String(), "not installed after apt-get install") {
		t.Fatalf("ok %v; log:\n%s", res.OK(), w.log.String())
	}
}

func TestAListWithABadNameIsRefusedWhole(t *testing.T) {
	w := newWorld(t)
	w.list("catatonit\nrm -rf /\n")
	if res := w.tick(); res.OK() || w.ran("apt-get") != 0 {
		t.Fatalf("ok %v, apt calls %d; want refused and nothing run", res.OK(), w.ran("apt-get"))
	}
}

func TestASecondTickChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.list("catatonit\n")
	w.tick()
	before := len(w.node.Calls())
	res := w.tick()
	if !res.OK() || w.log.Len() != 0 || w.ran("apt-get") != 2 || len(w.node.Calls()) != before+1 {
		t.Fatalf("ok %v, log %q, calls %v", res.OK(), w.log.String(), w.node.Calls())
	}
}
