package containers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
	"github.com/matta-kelly/monkeydo/units"
)

// getUser answers for any account with a home: an account's apps need no entry in nodes/<node>/accounts, since their
// git is nodes/<node>/users/<account>/apps/.
func (b *acctBox) getUser(name string) (string, error, bool) {
	if !b.exists {
		return "", errors.New("exit status 2"), true
	}
	return name + ":x:1002:1002::/home/" + name + ":/usr/sbin/nologin", nil, true
}

const (
	acctAppFile = "[Container]\nImage=ghcr.io/example/shop:sha-1\nContainerName=shop\n"
	acctAppDrop = "[Container]\nNetwork=data.network\nPublishPort=3070:3070\n"
	shopPin     = "shop  sha-1  ghcr.io/example/shop-quadlets  sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + "\n"
	acctFactory = "[Container]\nImage=assembly.build\n"
	acctBuild   = "[Build]\nImageTag=localhost/assembly\nFile=/x/Containerfile\n"
)

// accountAppWorld is the account "assembly" with the app shop. The account's side of its app is in git under apps/,
// beside its own Quadlets; the app's run files are its release on the node, and its pin is in releases. The account
// keeps a factory Quadlet of its own, so a tick that put nothing in would still render that.
func accountAppWorld(t *testing.T) *acctWorld {
	w := newAcctWorld(t)
	w.git("users/assembly/assembly.build", acctBuild)
	w.git("users/assembly/assembly.container", acctFactory)
	w.git("users/assembly/apps/shop/shop.container.d/node.conf", acctAppDrop)
	w.git("releases", shopPin)
	w.release("shop", "shop.container", acctAppFile)
	return w
}

// release puts an app's run files on the node, as the releases lane leaves them.
func (w *acctWorld) release(app, file, content string) {
	node.Write(w.box.n, filepath.Join(releaseDir, app, "current", file), node.File{Data: []byte(content), Mode: 0o644})
}

// appTick runs one lane for an account, as the command does.
func (w *acctWorld) appTick(account string) engine.Result {
	w.log.Reset()
	l := Lane{Node: w.box.n, Window: units.Window{For: 2, Step: 1}, Account: account}
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{l}, w.rec, &w.log)
}

// at is what the account reads as the account in the account's Quadlet folder.
func (w *acctWorld) at(account, file string) (string, bool) {
	data, err := os.ReadFile(w.box.n.Path("/home/" + account + "/.config/containers/systemd/" + file))
	return string(data), err == nil
}

func TestAnAccountsAppWhoseOwnFileSetsUserNSStillRuns(t *testing.T) {
	w := accountAppWorld(t)
	// the rule that an app's own file may not set UserNS is the system's alone: a system app is refused for it, an
	// account's runs, since a rootless container's UserNS is the account's own to set
	w.release("shop", "shop.container", acctAppFile+"UserNS=host\n")

	res := w.appTick("assembly")

	run, runOK := w.at("assembly", "shop.container")
	if !res.OK() || !runOK || run != acctAppFile+"UserNS=host\n" {
		t.Fatalf("ok %v, run %v %q", res.OK(), runOK, run)
	}
	if !contains(w.box.restarted, "shop.service") {
		t.Fatalf("the account's systemd did not restart shop: %v", w.box.restarted)
	}
}

func TestAnAccountsAppRunsFromItsReleaseBesideItsDropInsInTheAccountsScope(t *testing.T) {
	w := accountAppWorld(t)

	res := w.appTick("assembly")

	run, runOK := w.at("assembly", "shop.container")
	drop, dropOK := w.at("assembly", "shop.container.d/node.conf")
	if !res.OK() || !runOK || !dropOK || run != acctAppFile || drop != acctAppDrop {
		t.Fatalf("ok %v, run %v %q, drop %v %q", res.OK(), runOK, run, dropOK, drop)
	}
	if !contains(w.box.restarted, "shop.service") {
		t.Fatalf("the account's systemd did not restart shop: %v", w.box.restarted)
	}
	for _, c := range w.box.n.Calls() {
		if strings.Contains(c, "/home/assembly") && !strings.HasPrefix(c, "runuser -u assembly -- ") {
			t.Fatalf("root touched the account's home: %s", c)
		}
	}
	if _, err := os.Stat(w.box.n.Path("/etc/containers/systemd/shop.container")); err == nil {
		t.Fatal("an account's app went into the system's folder")
	}
}

func TestANewReleaseOrDropInOfAnAccountsAppRestartsItsUnit(t *testing.T) {
	cases := map[string]func(w *acctWorld){
		"a new release": func(w *acctWorld) {
			w.release("shop", "shop.container", strings.Replace(acctAppFile, "sha-1", "sha-2", 1))
		},
		"a new drop-in": func(w *acctWorld) {
			w.git("users/assembly/apps/shop/shop.container.d/node.conf", acctAppDrop+"Environment=SHOP=1\n")
		},
	}
	for name, change := range cases {
		w := accountAppWorld(t)
		if res := w.appTick("assembly"); !res.OK() {
			t.Fatalf("%s: the first tick did not hold", name)
		}
		w.box.restarted = nil
		change(w)

		res := w.appTick("assembly")

		if !res.OK() || !contains(w.box.restarted, "shop.service") {
			t.Fatalf("%s: ok %v, restarted %v", name, res.OK(), w.box.restarted)
		}
		if contains(w.box.restarted, "assembly.service") {
			t.Fatalf("%s: the account's own unit restarted too", name)
		}
	}
}

func TestADropInOfAnAccountsAppThatDoesNotSettleIsPutBackAlone(t *testing.T) {
	w := accountAppWorld(t)
	w.appTick("assembly")
	if _, ok := w.at("assembly", "assembly.container"); !ok {
		t.Fatal("the account's own Quadlet was not written")
	}
	w.box.sickWhen = "Environment=SHOP=bad"
	w.git("users/assembly/apps/shop/shop.container.d/node.conf", acctAppDrop+"Environment=SHOP=bad\n")

	res := w.appTick("assembly")

	if drop, _ := w.at("assembly", "shop.container.d/node.conf"); res.OK() || drop != acctAppDrop {
		t.Fatalf("ok %v, drop-in %q", res.OK(), drop)
	}
	if _, ok := w.at("assembly", "assembly.container"); !ok {
		t.Fatal("the account's own Quadlet was undone with the app's change")
	}
}

func TestANewReleaseOfAnAccountsAppThatDoesNotSettleIsPutBackAlone(t *testing.T) {
	w := accountAppWorld(t)
	w.appTick("assembly")
	w.box.sickWhen = "sha-2"
	w.release("shop", "shop.container", strings.Replace(acctAppFile, "sha-1", "sha-2", 1))

	res := w.appTick("assembly")

	if run, _ := w.at("assembly", "shop.container"); res.OK() || run != acctAppFile {
		t.Fatalf("ok %v, run %q", res.OK(), run)
	}
}

func TestAnAppThatLeavesAnAccountsGitIsStoppedAndRemovedFromIt(t *testing.T) {
	w := accountAppWorld(t)
	w.appTick("assembly")
	os.RemoveAll(w.checkout + "/nodes/mutable/n/users/assembly/apps")

	res := w.appTick("assembly")

	_, run := w.at("assembly", "shop.container")
	_, drop := w.at("assembly", "shop.container.d/node.conf")
	if !res.OK() || run || drop || !contains(w.box.stopped, "shop.service") {
		t.Fatalf("ok %v, run %v, drop %v, stopped %v", res.OK(), run, drop, w.box.stopped)
	}
	if _, err := os.Stat(w.box.n.Path("/home/assembly/.config/containers/systemd/shop.container.d")); err == nil {
		t.Fatal("the app's empty drop-in folder was left behind")
	}
	if _, ok := w.at("assembly", "assembly.container"); !ok {
		t.Fatal("the account's own Quadlet went with the app")
	}
}

func TestCheckRefusesAnAccountsAppForWhatASystemAppsCheckRefuses(t *testing.T) {
	cases := map[string]func(w *acctWorld){
		"no pin in releases": func(w *acctWorld) {
			os.Remove(w.checkout + "/nodes/mutable/n/releases")
		},
		"something in its folder that is not a drop-in": func(w *acctWorld) {
			w.git("users/assembly/apps/shop/notes.txt", "x\n")
		},
		"a drop-in folder that is not <file>.<kind>.d": func(w *acctWorld) {
			w.git("users/assembly/apps/shop/shop.container.d/node.txt", "[Container]\n")
		},
	}
	for name, setup := range cases {
		w := accountAppWorld(t)
		setup(w)
		// the good twin below counts the app it takes; this one must be refused, not passed over
		if n, err := CheckApps(engine.Source{Checkout: w.checkout, Node: "mutable/n"}); err == nil {
			t.Fatalf("%s: check passed, %d apps", name, n)
		}
	}
}

func TestCheckTakesAnAccountsAppWithoutAUserNamespaceDropIn(t *testing.T) {
	w := accountAppWorld(t)
	w.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\nNetwork=data.network\n")

	if n, err := CheckApps(engine.Source{Checkout: w.checkout, Node: "mutable/n"}); err != nil || n != 1 {
		t.Fatalf("an account's app with no UserNS=auto drop-in: counted %d, refused %v; the rule is the system's alone", n, err != nil)
	}
}
