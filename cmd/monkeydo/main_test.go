package main

import (
	"bytes"
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
	checkout, _, _ := setup(t, "crate/machine.toml")
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
