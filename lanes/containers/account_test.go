package containers

import (
	"bytes"
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

const home = "/home/assembly"

// acctBox stands in for one account: getent, runuser on files under the fake root, and the account's systemd.
type acctBox struct {
	n         *node.Fake
	exists    bool
	restarted []string
	stopped   []string
	sickWhen  string
}

func (b *acctBox) do(line string, stdin []byte) (string, error, bool) {
	f := strings.Fields(line)
	switch {
	case f[0] == "getent":
		if b.exists {
			return "assembly:x:1002:1002::" + home + ":/usr/sbin/nologin", nil, true
		}
		return "", errors.New("exit status 2"), true
	case f[0] == "runuser":
		if f[1] != "-u" || f[2] != "assembly" || f[3] != "--" {
			return "", errors.New("runuser as someone else"), true
		}
		return b.asAccount(f[4:], stdin)
	case strings.HasSuffix(f[0], "quadlet"):
		return "", nil, true
	case f[0] == "systemctl":
		if len(f) < 4 || f[1] != "--user" || f[3] != "assembly@" {
			return "", errors.New("not the account's systemd: " + line), true
		}
		args := f[4:]
		switch args[0] {
		case "restart":
			b.restarted = append(b.restarted, args[1])
		case "stop", "disable":
			b.stopped = append(b.stopped, args[len(args)-1])
		case "is-failed":
			data, _ := os.ReadFile(b.n.Path(home + "/.config/containers/systemd/" + strings.TrimSuffix(args[2], ".service") + ".container"))
			if b.sickWhen != "" && strings.Contains(string(data), b.sickWhen) {
				return "", nil, true
			}
			return "", errors.New("exit status 1"), true
		case "is-active":
			if len(args) == 2 {
				return "active", nil, true
			}
		case "show":
			if args[2] == "LoadState" {
				return "loaded", nil, true
			}
			return "1", nil, true
		}
		return "", nil, true
	}
	return "", nil, false
}

// asAccount does what a command run as the account would, on files under the fake root.
func (b *acctBox) asAccount(cmd []string, stdin []byte) (string, error, bool) {
	p := func(s string) string { return b.n.Path(s) }
	switch cmd[0] {
	case "test":
		info, err := os.Stat(p(cmd[2]))
		if err != nil || (cmd[1] == "-d") != info.IsDir() {
			return "", errors.New("exit status 1"), true
		}
		return "", nil, true
	case "cat":
		data, err := os.ReadFile(p(cmd[1]))
		return string(data), err, true
	case "find":
		entries, _ := os.ReadDir(p(cmd[1]))
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		return strings.Join(names, "\n"), nil, true
	case "mkdir":
		return "", os.MkdirAll(p(cmd[2]), 0o755), true
	case "sh":
		return "", os.WriteFile(p(cmd[len(cmd)-1]), stdin, 0o644), true
	case "rm":
		os.Remove(p(cmd[2]))
		return "", nil, true
	}
	return "", errors.New("unexpected: " + strings.Join(cmd, " ")), true
}

type acctWorld struct {
	t        *testing.T
	checkout string
	box      *acctBox
	rec      *engine.Record
	log      bytes.Buffer
}

func newAcctWorld(t *testing.T) *acctWorld {
	t.Helper()
	n := &node.Fake{Root: t.TempDir()}
	b := &acctBox{n: n, exists: true}
	n.Do = b.do
	os.MkdirAll(filepath.Dir(n.Path(gen)), 0o755)
	os.WriteFile(n.Path(gen), []byte("#!/bin/true\n"), 0o755)
	rec, _ := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	w := &acctWorld{t: t, checkout: t.TempDir(), box: b, rec: rec}
	w.git("vars.env", "DOMAIN=example.test\n")
	return w
}

func (w *acctWorld) git(rel, content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content), 0o644)
}

func (w *acctWorld) tick() engine.Result {
	w.log.Reset()
	l := Lane{Node: w.box.n, Window: units.Window{For: 2, Step: 1}, Account: "assembly"}
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{l}, w.rec, &w.log)
}

func (w *acctWorld) inHome(file string) (string, bool) {
	data, err := os.ReadFile(w.box.n.Path(home + "/.config/containers/systemd/" + file))
	return string(data), err == nil
}

const factory = "[Container]\nImage=assembly.build\n"
const build = "[Build]\nImageTag=localhost/assembly\nFile=/x/Containerfile\n"

func TestAnAccountsQuadletsGoInItsHomeOnlyAsTheAccount(t *testing.T) {
	w := newAcctWorld(t)
	w.git("users/assembly/assembly.container", factory)
	w.git("users/assembly/assembly.build", build)
	w.git("web.container", "[Container]\nImage=system\n") // the system's: not this lane's

	res := w.tick()

	if _, ok := w.inHome("assembly.container"); !ok || !res.OK() {
		t.Fatalf("on the account's folder %v, ok %v; log:\n%s", ok, res.OK(), w.log.String())
	}
	if _, ok := w.inHome("web.container"); ok {
		t.Fatal("a system quadlet went into the account's folder")
	}
	for _, c := range w.box.n.Calls() {
		if strings.Contains(c, home) && !strings.HasPrefix(c, "runuser -u assembly -- ") {
			t.Fatalf("root touched the account's home: %s", c)
		}
	}
}

func TestABuildGoesFirstAndItsContainerRestartsAfter(t *testing.T) {
	w := newAcctWorld(t)
	w.git("users/assembly/assembly.container", factory)
	w.git("users/assembly/assembly.build", build)
	w.tick()
	w.box.restarted = nil
	w.git("users/assembly/assembly.build", build+"Pull=always\n")

	res := w.tick()

	if !res.OK() || strings.Join(w.box.restarted, " ") != "assembly-build.service assembly.service" {
		t.Fatalf("ok %v, restarted %v; log:\n%s", res.OK(), w.box.restarted, w.log.String())
	}
}

func TestAnAccountNotOnTheNodeYetIsSkipped(t *testing.T) {
	w := newAcctWorld(t)
	w.box.exists = false
	w.git("users/assembly/assembly.container", factory)
	if res := w.tick(); !res.OK() || len(w.box.n.Calls()) != 1 {
		t.Fatalf("ok %v, calls %v; want one getent and nothing else", res.OK(), w.box.n.Calls())
	}
}

func TestAnAccountsUnitThatDoesNotSettleIsPutBackAsTheAccount(t *testing.T) {
	w := newAcctWorld(t)
	w.git("users/assembly/assembly.container", factory)
	w.git("users/assembly/assembly.build", build)
	w.tick()
	w.box.sickWhen = "Environment=BAD=1"
	w.git("users/assembly/assembly.container", factory+"Environment=BAD=1\n")

	res := w.tick()

	if data, _ := w.inHome("assembly.container"); res.OK() || strings.Contains(data, "BAD") {
		t.Fatalf("ok %v, in home %q; log:\n%s", res.OK(), data, w.log.String())
	}
}

func TestAccountsAreTheFoldersUnderUsers(t *testing.T) {
	w := newAcctWorld(t)
	w.git("users/assembly/assembly.container", factory)
	w.git("users/landl-db/db.container", factory)
	w.git("users/README.md", "x")
	got := Accounts(engine.Source{Checkout: w.checkout, Node: "mutable/n"})
	if strings.Join(got, ",") != "assembly,landl-db" {
		t.Fatalf("accounts = %v", got)
	}
}

func TestASecondTickOnAnAccountChangesNothing(t *testing.T) {
	w := newAcctWorld(t)
	w.git("users/assembly/assembly.container", factory)
	w.git("users/assembly/assembly.build", build)
	w.tick()
	w.box.restarted = nil
	res := w.tick()
	if !res.OK() || len(w.box.restarted) != 0 || w.log.Len() != 0 {
		t.Fatalf("ok %v, restarted %v, log:\n%s", res.OK(), w.box.restarted, w.log.String())
	}
}
