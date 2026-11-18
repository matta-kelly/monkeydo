package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
)

// accounts makes the world's node know accounts, each with a home at /home/<name>, and answer runuser as that account
// would: test, cat, rm, and the sh that writes a file, each on the fake node's files. wroteAs records who wrote what.
func (w *world) accounts(names ...string) map[string]string {
	wroteAs := map[string]string{}
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	inner := w.node.Do
	w.node.Do = func(line string, stdin []byte) (string, error, bool) {
		f := strings.Fields(line)
		switch {
		case f[0] == "getent" && f[1] == "passwd":
			if !known[f[2]] {
				return "", errors.New("exit status 2"), true
			}
			return f[2] + ":x:1001:1001::/home/" + f[2] + ":/bin/bash", nil, true
		case f[0] == "runuser":
			acct, cmd, path := f[2], f[4], f[len(f)-1]
			full := w.node.Path(path)
			switch cmd {
			case "test":
				if _, err := os.Stat(full); err != nil {
					return "", errors.New("exit status 1"), true
				}
				return "", nil, true
			case "cat":
				data, err := os.ReadFile(full)
				return string(data), err, true
			case "rm":
				os.Remove(full)
				return "", nil, true
			case "sh":
				os.MkdirAll(filepath.Dir(full), 0o700)
				wroteAs[path] = acct
				return "", os.WriteFile(full, stdin, 0o600), true
			}
		}
		return inner(line, stdin)
	}
	return wroteAs
}

func (w *world) files(content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "secrets", "files")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) onNode(path string) (string, os.FileMode, bool) {
	info, err := os.Stat(w.node.Path(path))
	if err != nil {
		return "", 0, false
	}
	data, _ := os.ReadFile(w.node.Path(path))
	return string(data), info.Mode().Perm(), true
}

const auth = `{"auths":{"ghcr.io":{"auth":"c2VjcmV0"}}}`

func TestASecretFileIsWrittenForRootAndAsEachAccount(t *testing.T) {
	w := newWorld(t)
	wroteAs := w.accounts("core", "assembly")
	w.seal("ghcr-auth", "sealed:"+auth)
	w.files("# the registry login\nghcr-auth /etc/dazoo/registry-auth.json\nghcr-auth ~core/.config/containers/auth.json\nghcr-auth  ~assembly/.config/containers/auth.json  # crate\n")

	res := w.tick()

	if !res.OK() {
		t.Fatalf("log:\n%s", w.log.String())
	}
	if data, mode, ok := w.onNode("/etc/dazoo/registry-auth.json"); !ok || data != auth || mode != 0o600 {
		t.Fatalf("root's file: %v %q %o", ok, data, mode)
	}
	for _, a := range []string{"core", "assembly"} {
		p := "/home/" + a + "/.config/containers/auth.json"
		if data, _, ok := w.onNode(p); !ok || data != auth || wroteAs[p] != a {
			t.Fatalf("%s's file: %v %q, written as %q", a, ok, data, wroteAs[p])
		}
	}
	if _, ok := w.box.secrets["ghcr-auth"]; ok {
		t.Fatal("a secret delivered as files was also loaded as a podman secret")
	}
	want := []string{"/etc/dazoo/registry-auth.json", "~assembly/.config/containers/auth.json", "~core/.config/containers/auth.json"}
	if !reflect.DeepEqual(w.rec.Owned("secrets"), want) {
		t.Fatalf("owned = %v", w.rec.Owned("secrets"))
	}
	if strings.Contains(w.log.String(), "c2VjcmV0") || strings.Contains(strings.Join(w.node.Calls(), "\n"), "c2VjcmV0") {
		t.Fatal("the value reached the log or a command line")
	}
}

func TestASecondTickChangesNoSecretFile(t *testing.T) {
	w := newWorld(t)
	w.accounts("core")
	w.seal("ghcr-auth", "sealed:"+auth)
	w.files("ghcr-auth /etc/dazoo/registry-auth.json\nghcr-auth ~core/.config/containers/auth.json\n")
	w.tick()
	if res := w.tick(); !res.OK() || w.log.Len() != 0 {
		t.Fatalf("ok %v, log %q", res.OK(), w.log.String())
	}
}

func TestARootFileWithTheWrongModeIsWrittenAgain(t *testing.T) {
	w := newWorld(t)
	w.seal("ghcr-auth", "sealed:"+auth)
	w.files("ghcr-auth /etc/dazoo/registry-auth.json\n")
	w.tick()
	os.Chmod(w.node.Path("/etc/dazoo/registry-auth.json"), 0o644)
	if res := w.tick(); !res.OK() || !strings.Contains(w.log.String(), "registry-auth.json: ok") {
		t.Fatalf("log:\n%s", w.log.String())
	}
	if _, mode, _ := w.onNode("/etc/dazoo/registry-auth.json"); mode != 0o600 {
		t.Fatalf("mode %o", mode)
	}
}

func TestAnAccountNotOnTheNodeYetWaitsForALaterTick(t *testing.T) {
	w := newWorld(t)
	w.accounts()
	w.seal("ghcr-auth", "sealed:"+auth)
	w.files("ghcr-auth ~core/.config/containers/auth.json\n")
	if res := w.tick(); !res.OK() || len(w.rec.Owned("secrets")) != 0 {
		t.Fatalf("ok %v, owned %v", res.OK(), w.rec.Owned("secrets"))
	}
}

func TestASecretFileThatLeavesGitIsRemoved(t *testing.T) {
	w := newWorld(t)
	w.accounts("core")
	w.seal("ghcr-auth", "sealed:"+auth)
	w.files("ghcr-auth /etc/dazoo/registry-auth.json\nghcr-auth ~core/.config/containers/auth.json\n")
	w.tick()
	w.files("ghcr-auth /etc/dazoo/registry-auth.json\n")

	res := w.tick()

	if _, _, ok := w.onNode("/home/core/.config/containers/auth.json"); !res.OK() || ok {
		t.Fatalf("ok %v, still there %v; log:\n%s", res.OK(), ok, w.log.String())
	}
	if !reflect.DeepEqual(w.rec.Owned("secrets"), []string{"/etc/dazoo/registry-auth.json"}) {
		t.Fatalf("owned = %v", w.rec.Owned("secrets"))
	}
}

func TestABadFilesListIsRefusedWhole(t *testing.T) {
	for _, bad := range []string{
		"ghcr-auth\n",                          // no path
		"ghcr-auth etc/x\n",                    // relative
		"ghcr-auth /etc/../root/x\n",           // not clean
		"ghcr-auth ~core/../../etc/shadow\n",   // out of the home
		"ghcr-auth ~root/x\n",                  // root's home is not an account's
		"nosuch /etc/x\n",                      // not sealed
		"ghcr-auth /etc/x\nghcr-auth /etc/x\n", // twice
	} {
		w := newWorld(t)
		w.accounts("core")
		w.seal("ghcr-auth", "sealed:"+auth)
		w.files(bad)
		if res := w.tick(); res.OK() {
			t.Fatalf("%q was not refused", bad)
		}
		if _, err := CheckFiles(engine.Source{Checkout: w.checkout, Node: "mutable/n"}); err == nil {
			t.Fatalf("CheckFiles passed %q", bad)
		}
	}
}
