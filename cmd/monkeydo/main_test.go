package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// setup is a checkout with one file for node mutable/n, a root and a state folder.
func setup(t *testing.T, rel string) (checkout, root, state string) {
	t.Helper()
	checkout, root, state = t.TempDir(), t.TempDir(), t.TempDir()
	p := filepath.Join(checkout, "nodes", "mutable", "n", "etc", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return
}

func call(args ...string) (int, string) {
	var out bytes.Buffer
	code := run(args, &out, &out)
	return code, out.String()
}

func TestRunWritesTheNodesFilesAndASecondRunDoesNothing(t *testing.T) {
	checkout, root, state := setup(t, "dazoo/x.conf")
	flags := []string{"--checkout", checkout, "--node", "mutable/n", "--root", root, "--state", state}

	code, out := call(append([]string{"run"}, flags...)...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if data, err := os.ReadFile(filepath.Join(root, "etc/dazoo/x.conf")); err != nil || string(data) != "x\n" {
		t.Fatalf("the file under root: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(state, "record.json")); err != nil {
		t.Fatalf("no record: %v", err)
	}

	code, out = call(append([]string{"run"}, flags...)...)
	if code != 0 || out != "" {
		t.Fatalf("second run: exit %d, out %q; want 0 and silent", code, out)
	}
}

func quadletIn(t *testing.T, checkout string) {
	t.Helper()
	p := filepath.Join(checkout, "nodes", "mutable", "n", "web.container")
	os.WriteFile(p, []byte("[Container]\nImage=x\n"), 0o644)
}

func TestCheckRefusesANodeThatRendersNoUnit(t *testing.T) {
	checkout, _, _ := setup(t, "dazoo/x.conf")
	code, out := call("check", "--checkout", checkout, "--node", "mutable/n")
	if code != 1 || !strings.Contains(out, "renders no unit file") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCheckRefusesAFileMonkeydoMayNotOwn(t *testing.T) {
	checkout, _, _ := setup(t, "dazoo/sync.env")
	code, out := call("check", "--checkout", checkout, "--node", "mutable/n")
	if code != 1 || !strings.Contains(out, "not monkeydo's") {
		t.Fatalf("exit %d: %s", code, out)
	}
	checkout, _, _ = setup(t, "dazoo/x.conf")
	quadletIn(t, checkout)
	if code, out := call("check", "--checkout", checkout, "--node", "mutable/n"); code != 0 {
		t.Fatalf("a good tree: exit %d: %s", code, out)
	}
}

func TestASecondTickWhileOneRunsStops(t *testing.T) {
	checkout, root, state := setup(t, "dazoo/x.conf")
	lock, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	code, out := call("run", "--checkout", checkout, "--node", "mutable/n", "--root", root, "--state", state)
	if code != 2 || !strings.Contains(out, "another tick") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestUsage(t *testing.T) {
	if code, _ := call(); code != 2 {
		t.Fatalf("no args: exit %d", code)
	}
	if code, _ := call("bogus"); code != 2 {
		t.Fatalf("an unknown command: exit %d", code)
	}
	if code, _ := call("run"); code != 2 {
		t.Fatalf("run with no flags: exit %d", code)
	}
	if code, out := call("version"); code != 0 || strings.TrimSpace(out) != "dev" {
		t.Fatalf("version: exit %d, %q", code, out)
	}
}

func TestAnUnknownLaneIsUsage(t *testing.T) {
	checkout, root, state := setup(t, "dazoo/x.conf")
	code, out := call("run", "--checkout", checkout, "--node", "mutable/n", "--root", root, "--state", state, "--lanes", "files,bogus")
	if code != 2 || !strings.Contains(out, `no lane "bogus"`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCheckRefusesABadPackageAccountOrUnitList(t *testing.T) {
	for file, line := range map[string]string{"packages": "rm -rf /", "accounts": "root", "units": "dazoo-sync.timer"} {
		checkout, _, _ := setup(t, "dazoo/x.conf")
		if err := os.WriteFile(filepath.Join(checkout, "nodes", "mutable", "n", file), []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out := call("check", "--checkout", checkout, "--node", "mutable/n")
		if code != 1 || !strings.Contains(out, file+":") {
			t.Fatalf("%s: exit %d: %s", file, code, out)
		}
	}
}

func TestCheckRendersEachAccountsQuadlets(t *testing.T) {
	checkout, _, _ := setup(t, "dazoo/x.conf")
	p := filepath.Join(checkout, "nodes", "mutable", "n", "users", "assembly", "a.container")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("[Container]\nEnvironment=K=${NOPE}\n"), 0o644)
	code, out := call("check", "--checkout", checkout, "--node", "mutable/n")
	if code != 1 || !strings.Contains(out, "containers@assembly:") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestCheckProvesEachRenderWithTheGeneratorItIsGiven(t *testing.T) {
	checkout, _, _ := setup(t, "dazoo/x.conf")
	for _, rel := range []string{"web.container", "users/assembly/a.container"} {
		p := filepath.Join(checkout, "nodes", "mutable", "n", rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("[Container]\nImage=x\n"), 0o644)
	}
	bin := t.TempDir()
	// a generator that writes down its arguments, and refuses a user render
	gen := filepath.Join(bin, "quadlet")
	log := filepath.Join(bin, "calls")
	os.WriteFile(gen, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n[ \"$1\" = -user ] && { echo 'a.container: unsupported key'; exit 1; }\nexit 0\n"), 0o755)

	code, out := call("check", "--checkout", checkout, "--node", "mutable/n", "--quadlet", gen)

	calls, _ := os.ReadFile(log)
	if code != 1 || !strings.Contains(out, "containers: 1 ok") || !strings.Contains(out, "containers@assembly: quadlet refused") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if string(calls) != "--dryrun\n-user --dryrun\n" {
		t.Fatalf("generator calls %q", calls)
	}
}

// checkable is a checkout to run `monkeydo check` on: a node with a Quadlet of its own, a release list with nothing
// in it, a registry that serves each release it is given, and a generator that accepts every render.
type checkable struct {
	t        *testing.T
	checkout string
	args     []string
	srv      *httptest.Server
	tarballs map[string][]byte // path on the registry -> the release's tarball
	accounts []string
}

func newCheckable(t *testing.T) *checkable {
	t.Helper()
	checkout, _, _ := setup(t, "dazoo/x.conf")
	gen := filepath.Join(t.TempDir(), "quadlet")
	// a generator that accepts every render, and says so when run with --help
	if err := os.WriteFile(gen, []byte("#!/bin/sh\necho --help >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &checkable{t: t, checkout: checkout, tarballs: map[string][]byte{}}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := c.tarballs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	t.Cleanup(c.srv.Close)
	checkHTTP = c.srv.Client()
	t.Cleanup(func() { checkHTTP = nil })
	c.git("admin.container", "[Container]\nImage=admin\n")
	c.git("releases", "")
	c.args = []string{"--checkout", checkout, "--node", "mutable/n", "--quadlet", gen}
	return c
}

func (c *checkable) git(rel, content string) {
	c.t.Helper()
	p := filepath.Join(c.checkout, "nodes", "mutable", "n", rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

// shopPin pins a release that no registry serves.
const shopPin = "shop sha-1 https://example.test/shop.tgz sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"

// release publishes an app's release with one run file on the registry, and pins it in git, as its repo's CI and
// Renovate leave it.
func (c *checkable) release(app, file, content string) {
	c.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: file, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write([]byte(content))
	tw.Close()
	gz.Close()
	path := "/" + app + ".tar.gz"
	c.tarballs[path] = buf.Bytes()
	sum := sha256.Sum256(buf.Bytes())
	c.git("releases", fmt.Sprintf("%s sha-1 %s%s sha256:%x\n", app, c.srv.URL, path, sum))
}

// onNode keeps the account on the node: a line in git's accounts, which the accounts lane makes on the node.
func (c *checkable) onNode(account string) {
	c.t.Helper()
	c.accounts = append(c.accounts, account)
	c.git("accounts", strings.Join(c.accounts, "\n")+"\n")
}

// check runs `monkeydo check`, and gives its exit code and what it said.
func (c *checkable) check() (int, string) { return call(append([]string{"check"}, c.args...)...) }

func TestCheckPassesAnAccountThatRunsOnlyApps(t *testing.T) {
	c := newCheckable(t)
	c.onNode("assembly")
	// nothing under the account's own folder in git: the lane installs its app's run file and drop-in there
	c.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\nNetwork=data.network\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")

	code, out := c.check()

	if code != 0 {
		t.Fatalf("an account of apps alone: exit %d:\n%s", code, out)
	}
}

func TestCheckStillCountsAScopeTheLaneLeavesToALaterTickAsNoUnit(t *testing.T) {
	// an account not on the node is not this node's scope: no tick applies it, so no unit of its app is this node's
	// to hold. The lane says so by installing nothing, which is not a scope it would empty; the check says so too.
	c := newCheckable(t) // git keeps no such account on the node, so the accounts lane never makes it
	c.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")

	code, out := c.check()

	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if want := "containers@assembly: 0 ok"; !strings.Contains(out, want) {
		t.Fatalf("want %q:\n%s", want, out)
	}
}

func TestCheckStillRefusesAClaimedScopeWhoseAppsAreNotPinned(t *testing.T) {
	// the app check still fires: a scope with an unpinned app does not pass, whichever lane its line refuses at first
	c := newCheckable(t)
	c.onNode("assembly")
	c.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")
	// the pin the app now lacks: its release is on the node, so the app check is green before this line and is what
	// changes, however the scope's own line reads it
	c.git("releases", "")

	code, out := c.check()

	if code != 1 {
		t.Fatalf("exit %d; an app with no pin passed:\n%s", code, out)
	}
	if want := "apps: app shop of assembly has no pin in releases"; !strings.Contains(out, want) {
		t.Fatalf("an app with no pin was refused, but not for its pin: want %q:\n%s", want, out)
	}
}

func TestCheckCountsAScopeOfAnAccountsAppAsTheUnitTheLaneInstalls(t *testing.T) {
	// the app's run file is the unit: the scope's line counts it, as the lane would install it, so a scope of one app
	// reads as one unit and is not called one that renders nothing
	c := newCheckable(t)
	c.onNode("assembly")
	c.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")

	code, out := c.check()

	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if want := "containers@assembly: 1 ok"; !strings.Contains(out, want) {
		t.Fatalf("want %q, the app's run file:\n%s", want, out)
	}
}

func TestCheckStillRefusesASystemWithNothingInGit(t *testing.T) {
	// the system's folder is the node's whatever git says, so the lane applies it and refuses to empty it
	c := newCheckable(t)
	c.onNode("assembly")
	os.Remove(filepath.Join(c.checkout, "nodes", "mutable", "n", "admin.container"))
	c.git("users/assembly/a.container", "[Container]\nImage=a\n")

	code, out := c.check()

	if code != 1 {
		t.Fatalf("exit %d; want 1:\n%s", code, out)
	}
	if want := "containers: renders no unit file"; !strings.Contains(out, want) {
		t.Fatalf("refused, but not at the system's containers lane: want %q:\n%s", want, out)
	}
}

func TestCheckKeepsTheSystemsRenderAsItIs(t *testing.T) {
	// an account's app changes nothing of the system's scope: the node's own Quadlet, its app's run file and drop-in
	c := newCheckable(t)
	c.git("apps/shop/shop.container.d/node.conf", "[Container]\nUserNS=auto\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")
	c.onNode("assembly")
	c.git("users/assembly/admin.container", "[Container]\nImage=a\n")

	code, out := c.check()

	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if want := "containers: 1 ok"; !strings.Contains(out, want) {
		t.Fatalf("containers: want %q, the system's own Quadlet:\n%s", want, out)
	}
}

func TestCheckKeepsTheSystemsCountOfItsOwnFolder(t *testing.T) {
	// the system's line counts the node's own Quadlets, as it always did: its app's units are the apps line's to prove,
	// and its app's run files must still be on the node for the scope to be one the lane would apply
	c := newCheckable(t)
	c.git("apps/shop/shop.container.d/node.conf", "[Container]\nUserNS=auto\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")

	code, out := c.check()

	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if want := "containers: 1 ok"; !strings.Contains(out, want) {
		t.Fatalf("containers: want %q, the system's own Quadlet:\n%s", want, out)
	}
}

func TestCheckCountsAnAccountNotOnTheNodeAsNoUnit(t *testing.T) {
	c := newCheckable(t)
	// git keeps another account on the node, and not this one
	c.onNode("landl-db")
	c.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\n")
	c.git("releases", shopPin)
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")

	code, out := c.check()

	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if want := "containers@assembly: 0 ok"; !strings.Contains(out, want) {
		t.Fatalf("want %q, the account the node does not have:\n%s", want, out)
	}
}

func TestCheckTakesAnAccountAppAsItTakesASystemApp(t *testing.T) {
	c := newCheckable(t)
	c.onNode("assembly")
	c.git("users/assembly/assembly.container", "[Container]\nImage=a\n")
	c.git("users/assembly/apps/shop/shop.container.d/node.conf", "[Container]\nNetwork=data.network\n")
	c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")

	if code, out := c.check(); code != 0 || !strings.Contains(out, "apps: 1 ok") {
		t.Fatalf("a good account app: exit %d:\n%s", code, out)
	}

	// the same break a system app is refused for: something in its folder that is not a drop-in
	c.git("users/assembly/apps/shop/notes.txt", "x\n")
	if code, out := c.check(); code != 1 || !strings.Contains(out, "apps:") {
		t.Fatalf("a bad account app: exit %d:\n%s", code, out)
	}
}

func TestCheckRendersAppsFromTheirPinsNeverFromTheMachineItRunsOn(t *testing.T) {
	// the verdict is the commit's: an app's run files come from its pinned release, fetched and proved by its digest,
	// so the same commit passes or fails the same on any machine, whatever it has installed
	for _, account := range []string{"", "assembly"} {
		c := newCheckable(t)
		dir := "apps/shop/shop.container.d/node.conf"
		drop := "[Container]\nUserNS=auto\n"
		if account != "" {
			c.onNode(account)
			dir, drop = "users/"+account+"/"+dir, "[Container]\n"
		}
		c.git(dir, drop)
		c.release("shop", "shop.container", "[Container]\nImage=shop:sha-1\n")
		if code, out := c.check(); code != 0 {
			t.Fatalf("%q: a pin the registry serves: exit %d:\n%s", account, code, out)
		}

		c.git("releases", shopPin) // a pin no registry serves

		code, out := c.check()
		if code != 1 {
			t.Fatalf("%q: a pin that cannot be fetched passed:\n%s", account, out)
		}
		if want := "node: the node the commit makes: shop sha-1 could not be fetched from its pin"; !strings.Contains(out, want) {
			t.Fatalf("%q: want %q:\n%s", account, want, out)
		}
	}
}

func TestCheckFetchesAPrivateReleaseWithTheRegistryLoginItIsGiven(t *testing.T) {
	// the login is the node's own place for it in the scratch root, so the check pulls as the node pulls, and it goes
	// with the root
	c := newCheckable(t)
	c.git("apps/shop/shop.container.d/node.conf", "[Container]\nUserNS=auto\n")
	c.git("releases", "shop 1 ghcr.example/o/shop-quadlets sha256:"+strings.Repeat("a", 64)+"\n")

	code, out := c.check()
	if code != 1 || !strings.Contains(out, "no registry login was given: --auth") {
		t.Fatalf("a private pin with no login: exit %d:\n%s", code, out)
	}

	auth := filepath.Join(t.TempDir(), "auth.json")
	os.WriteFile(auth, []byte(`{"auths":{"ghcr.example":{"auth":"dTpw"}}}`), 0o600)
	c.args = append(c.args, "--auth", auth)
	code, out = c.check()
	if code != 1 || strings.Contains(out, "no registry login was given") {
		t.Fatalf("with a login, the check did not say it had one: exit %d:\n%s", code, out)
	}
	left, _ := filepath.Glob(filepath.Join(os.TempDir(), "monkeydo-check-*"))
	for _, d := range left {
		if _, err := os.Stat(filepath.Join(d, "etc/dazoo/registry-auth.json")); err == nil {
			t.Fatalf("the login outlived the check, in %s", d)
		}
	}
}
