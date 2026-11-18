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

const gen = "/usr/libexec/podman/quadlet"

// sd is a stand-in for systemd and the quadlet generator. A unit is sick when its file on the node holds sickWhen.
type sd struct {
	n        *node.Fake
	refuse   bool            // the generator refuses the render
	sickWhen string          // a unit whose file holds this text fails after a restart
	started  map[string]bool // never-started units, and what was started
	never    map[string]bool
	stopped  []string
}

func (s *sd) fileOf(u string) string {
	base := strings.TrimSuffix(u, ".service")
	for _, ext := range []string{".container", ".network", ".volume"} {
		name := base
		if ext != ".container" {
			name = strings.TrimSuffix(base, "-"+ext[1:])
		}
		if data, err := os.ReadFile(s.n.Path("/etc/containers/systemd/" + name + ext)); err == nil {
			// a unit is its file and its drop-ins
			drops, _ := filepath.Glob(s.n.Path("/etc/containers/systemd/" + name + ext + ".d/*.conf"))
			for _, d := range drops {
				more, _ := os.ReadFile(d)
				data = append(data, more...)
			}
			return string(data)
		}
	}
	return ""
}

func (s *sd) do(line string, _ []byte) (string, error, bool) {
	f := strings.Fields(line)
	switch {
	case strings.HasSuffix(f[0], "quadlet"):
		if s.refuse {
			return "Loading source unit file /x/web.container\nweb.container: unsupported key 'Bogus'", errors.New("exit status 1"), true
		}
		return "", nil, true
	case f[0] != "systemctl":
		return "", nil, false
	case f[1] == "daemon-reload":
		return "", nil, true
	case f[1] == "restart" || f[1] == "start":
		s.started[f[len(f)-1]] = true
		return "", nil, true
	case f[1] == "stop" || f[1] == "disable":
		s.stopped = append(s.stopped, f[len(f)-1])
		return "", nil, true
	case f[1] == "is-failed":
		if s.sickWhen != "" && strings.Contains(s.fileOf(f[3]), s.sickWhen) {
			return "", nil, true
		}
		return "", errors.New("exit status 1"), true
	case f[1] == "is-active" && len(f) == 3:
		if s.never[f[2]] {
			return "inactive", errors.New("exit status 3"), true
		}
		return "active", nil, true
	case f[1] == "is-active":
		return "", nil, true
	case f[1] == "show" && f[3] == "LoadState":
		return "loaded", nil, true
	case f[1] == "show" && f[3] == "ActiveEnterTimestampMonotonic":
		if s.never[f[5]] {
			return "0", nil, true
		}
		return "123", nil, true
	case f[1] == "show":
		return "0", nil, true
	}
	return "", nil, false
}

type world struct {
	t        *testing.T
	checkout string
	sd       *sd
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	n := &node.Fake{Root: t.TempDir()}
	s := &sd{n: n, started: map[string]bool{}, never: map[string]bool{}}
	n.Do = s.do
	os.MkdirAll(filepath.Dir(n.Path(gen)), 0o755)
	os.WriteFile(n.Path(gen), []byte("#!/bin/true\n"), 0o755)
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, checkout: t.TempDir(), sd: s, rec: rec}
	w.git("vars.env", "DOMAIN=example.test\n")
	return w
}

func (w *world) git(rel, content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}
func (w *world) ungit(rel string) { os.Remove(filepath.Join(w.checkout, "nodes", "mutable", "n", rel)) }

func (w *world) box(name, content string) {
	node.Write(w.sd.n, "/etc/containers/systemd/"+name, node.File{Data: []byte(content), Mode: 0o644})
}
func (w *world) onBox(name string) (string, bool) {
	data, err := os.ReadFile(w.sd.n.Path("/etc/containers/systemd/" + name))
	return string(data), err == nil
}

func (w *world) tick() engine.Result {
	w.log.Reset()
	l := Lane{Node: w.sd.n, Window: units.Window{For: 2, Step: 1}}
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{l}, w.rec, &w.log)
}

const web = "[Container]\nImage=docker.io/library/nginx@sha256:abc\nLabel=host=web.${DOMAIN}\n"

func TestANewContainerIsRenderedWrittenAndRestartedHealthy(t *testing.T) {
	w := newWorld(t)
	w.git("web/web.container", web)

	res := w.tick()

	data, ok := w.onBox("web.container")
	if !res.OK() || !ok || !strings.Contains(data, "host=web.example.test") {
		t.Fatalf("ok %v, on box %v %q; log:\n%s", res.OK(), ok, data, w.log.String())
	}
	if !w.sd.started["web.service"] {
		t.Fatal("web.service was not restarted")
	}
}

func TestNetworksAndVolumesGoBeforeContainers(t *testing.T) {
	w := newWorld(t)
	w.git("a.container", "[Container]\nImage=x\n")
	w.git("z.network", "[Network]\n")
	w.tick()
	var order []string
	for _, c := range w.sd.n.Calls() {
		if strings.HasPrefix(c, "systemctl restart") {
			order = append(order, strings.Fields(c)[2])
		}
	}
	if strings.Join(order, " ") != "z-network.service a.service" {
		t.Fatalf("restart order %v", order)
	}
}

func TestAChangeThatDoesNotSettleIsPutBackAlone(t *testing.T) {
	w := newWorld(t)
	w.git("web.container", web)
	w.git("api.container", "[Container]\nImage=api:1\n")
	w.tick()
	w.sd.sickWhen = "Image=api:2"
	w.git("api.container", "[Container]\nImage=api:2\n")
	w.git("web.container", web+"Environment=X=1\n")

	res := w.tick()

	if data, _ := w.onBox("api.container"); res.OK() || !strings.Contains(data, "api:1") {
		t.Fatalf("ok %v, api on box %q; log:\n%s", res.OK(), data, w.log.String())
	}
	if data, _ := w.onBox("web.container"); !strings.Contains(data, "X=1") {
		t.Fatal("a healthy change was undone with the bad one")
	}
}

func TestANewContainerThatDoesNotSettleIsRemovedAndStopped(t *testing.T) {
	w := newWorld(t)
	w.sd.sickWhen = "Image=bad"
	w.git("web.container", "[Container]\nImage=bad\n")
	res := w.tick()
	if _, ok := w.onBox("web.container"); ok || res.OK() || !contains(w.sd.stopped, "web.service") {
		t.Fatalf("on box %v, ok %v, stopped %v", ok, res.OK(), w.sd.stopped)
	}
}

func TestAFileGitLacksIsStoppedBeforeItIsRemoved(t *testing.T) {
	w := newWorld(t)
	w.git("web.container", web)
	w.box("old.container", "[Container]\nImage=old\n")

	res := w.tick()

	if _, ok := w.onBox("old.container"); ok || !res.OK() || !contains(w.sd.stopped, "old.service") {
		t.Fatalf("still on box %v, ok %v, stopped %v", ok, res.OK(), w.sd.stopped)
	}
}

func TestARenderThatCannotBeProvedChangesNothing(t *testing.T) {
	cases := map[string]func(w *world){
		"a placeholder the node cannot fill": func(w *world) { w.git("ts.container", "[Container]\nEnvironment=KEY=${TS_AUTHKEY}\n") },
		"the generator refuses it":           func(w *world) { w.git("web.container", web); w.sd.refuse = true },
		"nothing rendered":                   func(w *world) { w.git("README.md", "no quadlets\n") },
	}
	for name, setup := range cases {
		w := newWorld(t)
		w.box("keep.container", "[Container]\nImage=keep\n")
		setup(w)
		res := w.tick()
		if _, ok := w.onBox("keep.container"); res.OK() || !ok {
			t.Fatalf("%s: ok %v, kept %v; log:\n%s", name, res.OK(), ok, w.log.String())
		}
	}
}

func TestAPlaceholderInACommentIsFine(t *testing.T) {
	w := newWorld(t)
	w.git("web.container", "# the key comes from ${TS_AUTHKEY} at build time, never here\n"+web)
	if res := w.tick(); !res.OK() {
		t.Fatalf("log:\n%s", w.log.String())
	}
}

func TestAnAccountsQuadletsAreNotTheSystems(t *testing.T) {
	w := newWorld(t)
	w.git("web.container", web)
	w.git("users/assembly/factory.container", "[Container]\nImage=f\n")
	w.tick()
	if _, ok := w.onBox("factory.container"); ok {
		t.Fatal("a quadlet under users/ was put in the system's folder")
	}
}

func TestAContainerNeverStartedSinceBootIsStarted(t *testing.T) {
	w := newWorld(t)
	w.git("web.container", web)
	w.tick()
	w.sd.started = map[string]bool{}
	w.sd.never["web.service"] = true
	w.tick()
	if !w.sd.started["web.service"] {
		t.Fatal("a never-started unit was not started")
	}
}

func TestASecondTickChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.git("web.container", web)
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

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func TestAGeneratorRefusalNamesOnlyItsReason(t *testing.T) {
	out := "---web.service---\n[Unit]\nDescription=x\nquadlet-generator[1]: Loading source unit file /units/t.container\n" +
		"quadlet-generator[1]: Warning: t.container specifies the image \"x\" which not a fully qualified image name.\n" +
		"quadlet-generator[1]: converting \"t.container\": unsupported key 'BogusKey' in group 'Container' in /units/t.container\n"
	if got := reasons(out); got != `converting "t.container": unsupported key 'BogusKey' in group 'Container' in /units/t.container` {
		t.Fatalf("reasons = %q", got)
	}
}
