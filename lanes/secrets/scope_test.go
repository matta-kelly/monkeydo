package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pods makes the world's node know accounts, each with a home at /home/<name> and uid 1001, a rootless podman of its
// own, Quadlets read as the account, and a systemd reached as systemctl --user -M <name>@. It answers each as the account
// would, and records who ran podman (podmanAs) and what each account's unit started with (startedAs, "<account> <unit>").
type pods struct {
	secrets   map[string]map[string]string // account -> its podman secrets
	claims    map[string]string            // "<account> <unit>" -> the secret it claims
	startedAs map[string]string
	podmanAs  map[string]bool // every podman call ran as an account with its runtime folder
}

func (w *world) pods(names ...string) *pods {
	p := &pods{secrets: map[string]map[string]string{}, claims: map[string]string{}, startedAs: map[string]string{}, podmanAs: map[string]bool{}}
	for _, n := range names {
		p.secrets[n] = map[string]string{}
	}
	inner := w.node.Do
	w.node.Do = func(line string, stdin []byte) (string, error, bool) {
		f := strings.Fields(line)
		switch {
		case f[0] == "getent" && f[1] == "passwd":
			if _, ok := p.secrets[f[2]]; !ok {
				return "", errors.New("exit status 2"), true
			}
			return f[2] + ":x:1001:1001::/home/" + f[2] + ":/bin/bash", nil, true
		case f[0] == "runuser" && f[4] == "env" && f[6] == "podman":
			acct := f[2]
			p.podmanAs[acct] = f[5] == "XDG_RUNTIME_DIR=/run/user/1001"
			mine, sub := p.secrets[acct], f[7:]
			switch {
			case sub[1] == "inspect" && sub[2] == "--showsecret":
				if v, ok := mine[sub[5]]; ok {
					return v + "\n", nil, true
				}
				return "", errors.New("no such secret"), true
			case sub[1] == "inspect":
				if _, ok := mine[sub[2]]; ok {
					return "", nil, true
				}
				return "", errors.New("no such secret"), true
			case sub[1] == "rm":
				delete(mine, sub[2])
				return "", nil, true
			case sub[1] == "create":
				mine[sub[2]] = string(stdin)
				return "", nil, true
			}
		case f[0] == "runuser" && f[4] == "find":
			files, _ := filepath.Glob(filepath.Join(w.node.Path(f[5]), "*.container"))
			var out []string
			for _, file := range files {
				out = append(out, filepath.Base(file))
			}
			return strings.Join(out, "\n"), nil, true
		case f[0] == "runuser" && f[4] == "cat":
			data, err := os.ReadFile(w.node.Path(f[5]))
			return string(data), err, true
		case f[0] == "systemctl" && f[1] == "--user":
			acct := strings.TrimSuffix(f[3], "@")
			switch f[4] {
			case "restart":
				key := acct + " " + f[5]
				p.startedAs[key] = p.secrets[acct][p.claims[key]]
				return "", nil, true
			case "is-failed":
				return "", errors.New("exit status 1"), true
			case "is-active":
				return "", nil, true
			case "show":
				return "0", nil, true
			}
		}
		return inner(line, stdin)
	}
	return p
}

// accountQuadlet puts a container in account's Quadlets that claims secret.
func (w *world) accountQuadlet(p *pods, account, unit, secret string) {
	path := w.node.Path("/home/" + account + "/.config/containers/systemd/" + unit + ".container")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("[Container]\nImage=x\nSecret="+secret+",type=env,target=TOKEN\n"), 0o644)
	p.claims[account+" "+unit+".service"] = secret
}

func TestAnAccountsSecretFromOpenBaoIsLoadedIntoItsOwnPodmanAndItsConsumerRestarted(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	p := w.pods("assembly")
	w.accountQuadlet(p, "assembly", "assembly", "fireworks-api-key")
	w.quadlet("radarr", "fireworks-api-key") // root's container of the same name claims root's secret, untouched
	s.put("fireworks/api", "api_key", "m0del")
	w.declare(s, "assembly/fireworks-api-key fireworks/api api_key")

	res := w.tickWith(s)

	if !res.OK() || p.secrets["assembly"]["fireworks-api-key"] != "m0del" || p.startedAs["assembly assembly.service"] != "m0del" {
		t.Fatalf("ok %v, secret %q, started %q; log:\n%s", res.OK(), p.secrets["assembly"]["fireworks-api-key"], p.startedAs["assembly assembly.service"], w.log.String())
	}
	if !p.podmanAs["assembly"] {
		t.Fatal("the account's podman ran without its runtime folder")
	}
	if _, ok := w.box.secrets["fireworks-api-key"]; ok {
		t.Fatal("an account's secret was loaded into root's podman")
	}
	if _, ok := w.box.started["radarr.service"]; ok {
		t.Fatal("root's container of the same name was restarted")
	}
	for _, c := range w.node.Calls() {
		if strings.Contains(c, "m0del") {
			t.Fatalf("a value is in a command line: %s", c)
		}
	}
	n := len(w.node.Calls())
	if res := w.tickWith(s); !res.OK() {
		t.Fatalf("a second tick: %s", w.log.String())
	}
	for _, c := range w.node.Calls()[n:] {
		if strings.Contains(c, " create ") || strings.Contains(c, " restart ") {
			t.Fatalf("a second tick changed an unchanged secret: %s", c)
		}
	}
}

func TestAnAccountNotOnTheNodeYetGetsItsSecretOnALaterTick(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	p := w.pods()
	s.put("fireworks/api", "api_key", "m0del")
	w.declare(s, "assembly/fireworks-api-key fireworks/api api_key")

	if res := w.tickWith(s); !res.OK() {
		t.Fatalf("a missing account failed the tick:\n%s", w.log.String())
	}
	p.secrets["assembly"] = map[string]string{}
	if res := w.tickWith(s); !res.OK() || p.secrets["assembly"]["fireworks-api-key"] != "m0del" {
		t.Fatalf("ok %v, secret %q; log:\n%s", res.OK(), p.secrets["assembly"]["fireworks-api-key"], w.log.String())
	}
}

func TestAnAccountNameThatIsNoAccountIsRefused(t *testing.T) {
	for _, name := range []string{"root/x", "Bad/x", "assembly/", "/x"} {
		w, s := newWorld(t), newStore(t)
		w.declare(s, name+" a/b f")
		if res := w.tickWith(s); res.OK() || !strings.Contains(w.log.String(), "secrets/openbao") {
			t.Fatalf("%s: ok %v; log:\n%s", name, res.OK(), w.log.String())
		}
	}
}

func TestAFileFromOpenBaoIsDeliveredAndKeptWhileTheStoreCannotGiveIt(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	s.put("github/ghcr-push", "auth_json", `{"auths":{}}`)
	w.declare(s, "ghcr-push-auth github/ghcr-push auth_json")
	w.files("ghcr-push-auth /etc/crate/push-auth.json\n")

	res := w.tickWith(s)

	got, _ := os.ReadFile(w.node.Path("/etc/crate/push-auth.json"))
	if !res.OK() || string(got) != `{"auths":{}}` {
		t.Fatalf("ok %v, file %q; log:\n%s", res.OK(), got, w.log.String())
	}
	if _, ok := w.box.secrets["ghcr-push-auth"]; ok {
		t.Fatal("a secret delivered as a file was also loaded as a podman secret")
	}

	s.denied["github/ghcr-push"] = true
	res = w.tickWith(s)

	got, err := os.ReadFile(w.node.Path("/etc/crate/push-auth.json"))
	if res.OK() || err != nil || string(got) != `{"auths":{}}` {
		t.Fatalf("with the store refusing: ok %v, file %q (%v); log:\n%s", res.OK(), got, err, w.log.String())
	}
}
