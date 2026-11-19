package containers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

const (
	appFile = "[Container]\nImage=ghcr.io/example/shop:sha-1\nContainerName=shop\n"
	appDrop = "[Container]\nUserNS=auto\nNetwork=data.network\nPublishPort=3070:3070\n"
	pin     = "shop  sha-1  ghcr.io/example/shop-quadlets  sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + "\n"
)

// release puts an app's run files on the node, as the releases lane leaves them.
func (w *world) release(app, file, content string) {
	node.Write(w.sd.n, filepath.Join(releaseDir, app, "current", file), node.File{Data: []byte(content), Mode: 0o644})
}

// app is a node with one dazoo quadlet and the app shop: its release on the node, and its drop-in and pin in git.
func appWorld(t *testing.T) *world {
	w := newWorld(t)
	w.git("web.container", web)
	w.git("releases", pin)
	w.git("apps/shop/shop.container.d/node.conf", appDrop)
	w.release("shop", "shop.container", appFile)
	return w
}

func TestAnAppsRunFilesAndDropInsAreWrittenAndItsUnitRestarted(t *testing.T) {
	w := appWorld(t)
	res := w.tick()
	file, ok1 := w.onBox("shop.container")
	drop, ok2 := w.onBox("shop.container.d/node.conf")
	if !res.OK() || !ok1 || !ok2 || file != appFile || drop != appDrop {
		t.Fatalf("ok %v, file %v %q, drop-in %v %q; log:\n%s", res.OK(), ok1, file, ok2, drop, w.log.String())
	}
	if !w.sd.started["shop.service"] {
		t.Fatal("shop.service was not restarted")
	}
	n := 0
	for _, c := range w.sd.n.Calls() {
		if c == "systemctl restart shop.service" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("shop.service restarted %d times, not once", n)
	}
}

func TestASystemAppIsRefusedBeforeAnythingChanges(t *testing.T) {
	cases := map[string]func(w *world){
		"a container with no drop-in":  func(w *world) { w.release("shop", "cart.container", appFile) },
		"its own file sets UserNS":     func(w *world) { w.release("shop", "shop.container", appFile+"UserNS=host\n") },
		"a drop-in not UserNS=auto":    func(w *world) { w.git("apps/shop/shop.container.d/node.conf", "[Container]\nUserNS=host\n") },
		"a drop-in for a missing file": func(w *world) { w.git("apps/shop/gone.container.d/node.conf", appDrop) },
		"a name dazoo has too":         func(w *world) { w.git("shop.container", web) },
		"its release not on the node":  func(w *world) { os.RemoveAll(w.sd.n.Path(releaseDir)) },
		"a stray file in its folder":   func(w *world) { w.git("apps/shop/notes.txt", "x\n") },
	}
	for name, setup := range cases {
		w := appWorld(t)
		w.box("keep.container", "[Container]\nImage=keep\n")
		setup(w)
		res := w.tick()
		if _, ok := w.onBox("keep.container"); res.OK() || !ok {
			t.Fatalf("%s: ok %v, kept %v; log:\n%s", name, res.OK(), ok, w.log.String())
		}
		if _, ok := w.onBox("shop.container"); ok {
			t.Fatalf("%s: the app was written", name)
		}
	}
}

func TestADropInThatBreaksItsUnitIsPutBack(t *testing.T) {
	w := appWorld(t)
	w.tick()
	w.sd.sickWhen = "PublishPort=9999"
	w.git("apps/shop/shop.container.d/node.conf", strings.Replace(appDrop, "3070:3070", "9999:9999", 1))

	res := w.tick()

	if drop, _ := w.onBox("shop.container.d/node.conf"); res.OK() || drop != appDrop {
		t.Fatalf("ok %v, drop-in %q; log:\n%s", res.OK(), drop, w.log.String())
	}
}

func TestANewReleaseRewritesAndRestartsTheAppsUnit(t *testing.T) {
	w := appWorld(t)
	w.tick()
	w.sd.started = map[string]bool{}
	w.release("shop", "shop.container", strings.Replace(appFile, "sha-1", "sha-2", 1))

	res := w.tick()

	if file, _ := w.onBox("shop.container"); !res.OK() || !strings.Contains(file, "sha-2") || !w.sd.started["shop.service"] {
		t.Fatalf("ok %v, file %q, restarted %v", res.OK(), file, w.sd.started["shop.service"])
	}
	if w.sd.started["web.service"] {
		t.Fatal("a unit the release does not touch was restarted")
	}
}

func TestASecondTickWithAnAppChangesNothing(t *testing.T) {
	w := appWorld(t)
	w.tick()
	n := len(w.sd.n.Calls())
	res := w.tick()
	for _, c := range w.sd.n.Calls()[n:] {
		if strings.Contains(c, "restart") || strings.Contains(c, "daemon-reload") || strings.Contains(c, "disable") {
			t.Fatalf("a second tick changed something: %s", c)
		}
	}
	if !res.OK() || w.log.Len() != 0 {
		t.Fatalf("ok %v, log %q", res.OK(), w.log.String())
	}
}

func TestAnAppThatLeavesGitIsStoppedAndRemovedWithItsDropIns(t *testing.T) {
	w := appWorld(t)
	w.tick()
	os.RemoveAll(filepath.Join(w.checkout, "nodes", "mutable", "n", "apps"))

	res := w.tick()

	_, f := w.onBox("shop.container")
	_, err := os.Stat(w.sd.n.Path("/etc/containers/systemd/shop.container.d"))
	d := err == nil
	if !res.OK() || f || d || !contains(w.sd.stopped, "shop.service") {
		t.Fatalf("ok %v, file %v, drop-in folder %v, stopped %v; log:\n%s", res.OK(), f, d, w.sd.stopped, w.log.String())
	}
}

func TestTheSystemsAppsAndAnAccountsAppRunInTheirOwnScopes(t *testing.T) {
	w := appWorld(t)
	// the same app, in an account's git: the system's tick must not see it, and an account's must see only its own
	w.git("users/assembly/apps/other/other.container.d/node.conf", appDrop)
	w.release("other", "other.container", appFile)

	res := w.tick()

	if file, ok := w.onBox("shop.container"); !res.OK() || !ok || file != appFile {
		t.Fatalf("the system's own app: ok %v, %v %q; log:\n%s", res.OK(), ok, file, w.log.String())
	}
	if _, ok := w.onBox("other.container"); ok {
		t.Fatal("an account's app went into the system's folder")
	}
	if n, err := CheckApps(engine.Source{Checkout: w.checkout, Node: "mutable/n"}); n != 1 || err != nil {
		t.Fatalf("check saw %d apps of the system's, %v; want the one in apps/", n, err)
	}
}

func TestCheckAppsWantsAPinAndUserNSAuto(t *testing.T) {
	w := appWorld(t)
	src := engine.Source{Checkout: w.checkout, Node: "mutable/n"}
	if n, err := CheckApps(src); err != nil || n != 1 {
		t.Fatalf("a good app: %d, %v", n, err)
	}
	w.ungit("releases")
	if _, err := CheckApps(src); err == nil || !strings.Contains(err.Error(), "no pin") {
		t.Fatalf("an app with no pin: %v", err)
	}
	w.git("releases", pin)
	w.git("apps/shop/shop.container.d/node.conf", "[Container]\nNetwork=data.network\n")
	if _, err := CheckApps(src); err == nil || !strings.Contains(err.Error(), "UserNS=auto") {
		t.Fatalf("a drop-in without UserNS=auto: %v", err)
	}
}
