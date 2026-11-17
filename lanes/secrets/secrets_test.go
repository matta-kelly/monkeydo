package secrets

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
	"github.com/matta-kelly/monkeydo/units"
)

// box is a stand-in node: sops opens a file whose content is "sealed:<value>", podman keeps secrets in memory, and
// each unit comes up healthy unless its name is in sick.
type box struct {
	secrets  map[string]string
	sick     map[string]bool // a unit that fails while the value it got is the one named
	sickWith map[string]string
	started  map[string]string // unit -> the value of its secret when it last started
	claims   map[string]string // unit -> the secret it claims
}

func (b *box) do(line string, stdin []byte) (string, error, bool) {
	f := strings.Fields(line)
	switch {
	case f[0] == "sops":
		data, err := os.ReadFile(f[2])
		if err != nil || !bytes.HasPrefix(data, []byte("sealed:")) {
			return "", errors.New("no age key could open it"), true
		}
		return string(data[len("sealed:"):]), nil, true
	case f[0] == "podman" && f[2] == "inspect":
		if _, ok := b.secrets[f[3]]; ok {
			return "", nil, true
		}
		return "", errors.New("no such secret"), true
	case f[0] == "podman" && f[2] == "rm":
		delete(b.secrets, f[3])
		return "", nil, true
	case f[0] == "podman" && f[2] == "create":
		b.secrets[f[3]] = string(stdin)
		return "", nil, true
	case f[0] == "systemctl" && f[1] == "restart":
		b.started[f[2]] = b.secrets[b.claims[f[2]]]
		return "", nil, true
	case f[0] == "systemctl" && f[1] == "is-failed":
		u := f[3]
		if b.sick[u] && b.started[u] == b.sickWith[u] {
			return "", nil, true
		}
		return "", errors.New("exit status 1"), true
	case f[0] == "systemctl" && f[1] == "is-active":
		return "", nil, true
	case f[0] == "systemctl" && f[1] == "show":
		return "0", nil, true
	}
	return "", nil, false
}

type world struct {
	t        *testing.T
	checkout string
	box      *box
	node     *node.Fake
	rec      *engine.Record
	log      bytes.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	b := &box{secrets: map[string]string{}, sick: map[string]bool{}, sickWith: map[string]string{}, started: map[string]string{}, claims: map[string]string{}}
	rec, err := engine.LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, checkout: t.TempDir(), box: b, node: &node.Fake{Root: t.TempDir(), Do: b.do}, rec: rec}
}

func (w *world) seal(name, content string) {
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "secrets", "sealed", name+".sops")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// quadlet puts a container on the node that claims secret.
func (w *world) quadlet(unit, secret string) {
	p := w.node.Path("/etc/containers/systemd/" + unit + ".container")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("[Container]\nImage=x\nSecret="+secret+",type=env,target=TOKEN\n"), 0o644)
	w.box.claims[unit+".service"] = secret
}

func (w *world) tick() engine.Result {
	w.log.Reset()
	l := Lane{Node: w.node, Window: units.Window{For: 2, Step: 1}}
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{l}, w.rec, &w.log)
}

func (w *world) delivered(name string) string {
	data, _ := os.ReadFile(w.node.Path("/usr/share/dazoo/secrets/" + name + ".sops"))
	return string(data)
}

func TestANewSecretIsDeliveredLoadedAndItsConsumerRestarted(t *testing.T) {
	w := newWorld(t)
	w.quadlet("cloudflared", "cloudflare-token")
	w.seal("cloudflare-token", "sealed:t0ken")

	res := w.tick()

	if !res.OK() || w.box.secrets["cloudflare-token"] != "t0ken" || w.delivered("cloudflare-token") != "sealed:t0ken" {
		t.Fatalf("ok %v, secret %q, delivered %q; log:\n%s", res.OK(), w.box.secrets["cloudflare-token"], w.delivered("cloudflare-token"), w.log.String())
	}
	if w.box.started["cloudflared.service"] != "t0ken" {
		t.Fatal("the consumer was not restarted onto the new value")
	}
}

func TestAValueNeverReachesTheLogOrACommandLine(t *testing.T) {
	w := newWorld(t)
	w.quadlet("cloudflared", "cloudflare-token")
	w.seal("cloudflare-token", "sealed:hunter2")
	w.tick()
	for _, c := range w.node.Calls() {
		if strings.Contains(c, "hunter2") {
			t.Fatalf("a value is in a command line: %s", c)
		}
	}
	if strings.Contains(w.log.String(), "hunter2") {
		t.Fatalf("a value is in the log:\n%s", w.log.String())
	}
}

func TestARotationThatBreaksAConsumerPutsTheOldSecretBack(t *testing.T) {
	w := newWorld(t)
	w.quadlet("cloudflared", "cloudflare-token")
	w.seal("cloudflare-token", "sealed:old")
	w.tick()
	w.box.sick["cloudflared.service"] = true
	w.box.sickWith["cloudflared.service"] = "new"
	w.seal("cloudflare-token", "sealed:new")

	res := w.tick()

	if res.OK() || w.box.secrets["cloudflare-token"] != "old" || w.delivered("cloudflare-token") != "sealed:old" {
		t.Fatalf("ok %v, secret %q, delivered %q; log:\n%s", res.OK(), w.box.secrets["cloudflare-token"], w.delivered("cloudflare-token"), w.log.String())
	}
	if w.box.started["cloudflared.service"] != "old" {
		t.Fatal("the consumer was not restarted back onto the old value")
	}
}

func TestASecretThatDoesNotOpenRefusesTheLaneAndChangesNothing(t *testing.T) {
	w := newWorld(t)
	w.seal("a", "sealed:fine")
	w.seal("b", "sealed to another node's key")
	res := w.tick()
	if res.OK() || len(w.box.secrets) != 0 || !strings.Contains(w.log.String(), "b.sops does not open") {
		t.Fatalf("ok %v, secrets %v; log:\n%s", res.OK(), w.box.secrets, w.log.String())
	}
}

func TestAWrongSealIsRefused(t *testing.T) {
	w := newWorld(t)
	w.seal("a", `sealed:{"data": "the real value"}`)
	if res := w.tick(); res.OK() || len(w.box.secrets) != 0 || !strings.Contains(w.log.String(), "wrong seal") {
		t.Fatalf("ok %v; log:\n%s", res.OK(), w.log.String())
	}
}

func TestAnUnchangedSecretIsLeftAloneAndALostPodmanSecretComesBack(t *testing.T) {
	w := newWorld(t)
	w.quadlet("cloudflared", "cloudflare-token")
	w.seal("cloudflare-token", "sealed:t0ken")
	w.tick()
	n := len(w.node.Calls())
	if res := w.tick(); !res.OK() || w.log.Len() != 0 {
		t.Fatalf("a second tick: ok %v, log %q", res.OK(), w.log.String())
	}
	for _, c := range w.node.Calls()[n:] {
		if strings.Contains(c, "create") || strings.Contains(c, "restart") {
			t.Fatalf("a second tick changed something: %s", c)
		}
	}
	delete(w.box.secrets, "cloudflare-token")
	if res := w.tick(); !res.OK() || w.box.secrets["cloudflare-token"] != "t0ken" {
		t.Fatalf("a lost podman secret: ok %v, secret %q", res.OK(), w.box.secrets["cloudflare-token"])
	}
}

func TestASecretThatLeftGitIsLeftInPlace(t *testing.T) {
	w := newWorld(t)
	w.seal("a", "sealed:x")
	w.tick()
	os.Remove(filepath.Join(w.checkout, "nodes", "mutable", "n", "secrets", "sealed", "a.sops"))
	if res := w.tick(); !res.OK() || w.box.secrets["a"] != "x" || w.delivered("a") != "sealed:x" {
		t.Fatalf("ok %v, secrets %v", res.OK(), w.box.secrets)
	}
	if !reflect.DeepEqual(w.rec.Owned("secrets"), []string(nil)) && len(w.rec.Owned("secrets")) != 0 {
		t.Fatalf("owned = %v", w.rec.Owned("secrets"))
	}
}
