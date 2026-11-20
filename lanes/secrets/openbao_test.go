package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/units"
)

// store is a stand-in OpenBao: one AppRole login, a KV store, and the tokens it gave out.
type store struct {
	mu     sync.Mutex
	kv     map[string]map[string]string // path under secret/ -> fields
	denied map[string]bool              // a path the role may not read
	live   map[string]bool              // tokens not revoked
	logins int
	srv    *httptest.Server
}

func newStore(t *testing.T) *store {
	s := &store{kv: map[string]map[string]string{}, denied: map[string]bool{}, live: map[string]bool{}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *store) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deny := func() { w.WriteHeader(403); w.Write([]byte(`{"errors":["permission denied"]}`)) }
	switch {
	case r.URL.Path == "/v1/auth/approle/login":
		var c map[string]string
		json.NewDecoder(r.Body).Decode(&c)
		if c["role_id"] != "role" || c["secret_id"] != "s3cret-id" {
			w.WriteHeader(400)
			w.Write([]byte(`{"errors":["invalid role or secret ID"]}`))
			return
		}
		s.logins++
		tok := "tok-" + string(rune('a'+s.logins))
		s.live[tok] = true
		json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": tok}})
	case !s.live[r.Header.Get("X-Vault-Token")]:
		deny()
	case r.URL.Path == "/v1/auth/token/revoke-self":
		delete(s.live, r.Header.Get("X-Vault-Token"))
		w.WriteHeader(204)
	case strings.HasPrefix(r.URL.Path, "/v1/secret/data/"):
		p := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		if s.denied[p] {
			deny()
			return
		}
		f, ok := s.kv[p]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"errors":[]}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": f}})
	default:
		w.WriteHeader(404)
	}
}

func (s *store) put(path, field, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[path] = map[string]string{field: value}
}

// declare writes the node's secrets/openbao, with the store's address and the sealed login, and seals the login.
func (w *world) declare(s *store, lines ...string) {
	w.seal("openbao-approle", `sealed:{"role_id": "role", "secret_id": "s3cret-id"}`)
	body := "address " + s.srv.URL + "\nlogin openbao-approle\n" + strings.Join(lines, "\n") + "\n"
	p := filepath.Join(w.checkout, "nodes", "mutable", "n", "secrets", "openbao")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) tickWith(s *store) engine.Result {
	w.log.Reset()
	l := Lane{Node: w.node, Window: units.Window{For: 2, Step: 1}, HTTP: s.srv.Client()}
	return engine.Run(context.Background(), engine.Source{Checkout: w.checkout, Node: "mutable/n"}, []engine.Lane{l}, w.rec, &w.log)
}

func TestASecretFromOpenBaoIsLoadedAndItsConsumerRestarted(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	w.quadlet("radarr", "radarr-api-key")
	s.put("radarr/api", "api_key", "k3y")
	w.declare(s, "radarr-api-key radarr/api api_key")

	res := w.tickWith(s)

	if !res.OK() || w.box.secrets["radarr-api-key"] != "k3y" || w.box.started["radarr.service"] != "k3y" {
		t.Fatalf("ok %v, secret %q, started %q; log:\n%s", res.OK(), w.box.secrets["radarr-api-key"], w.box.started["radarr.service"], w.log.String())
	}
	if _, ok := w.box.secrets["openbao-approle"]; ok {
		t.Fatal("the login was loaded as a podman secret")
	}
	if len(s.live) != 0 {
		t.Fatalf("the tick left a token alive: %v", s.live)
	}
	for _, c := range w.node.Calls() {
		if strings.Contains(c, "k3y") || strings.Contains(c, "s3cret-id") || strings.Contains(c, "tok-") {
			t.Fatalf("a value is in a command line: %s", c)
		}
	}
	if strings.Contains(w.log.String(), "k3y") || strings.Contains(w.log.String(), "s3cret-id") {
		t.Fatalf("a value is in the log:\n%s", w.log.String())
	}
}

func TestAnUnchangedOpenBaoSecretIsLeftAloneAndARotationIsPickedUp(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	w.quadlet("radarr", "radarr-api-key")
	s.put("radarr/api", "api_key", "old")
	w.declare(s, "radarr-api-key radarr/api api_key")
	w.tickWith(s)

	n := len(w.node.Calls())
	if res := w.tickWith(s); !res.OK() || w.log.Len() != 0 {
		t.Fatalf("a second tick: ok %v, log %q", res.OK(), w.log.String())
	}
	for _, c := range w.node.Calls()[n:] {
		if strings.Contains(c, "create") || strings.Contains(c, "restart") {
			t.Fatalf("a second tick changed something: %s", c)
		}
	}

	s.put("radarr/api", "api_key", "new")
	if res := w.tickWith(s); !res.OK() || w.box.started["radarr.service"] != "new" {
		t.Fatalf("a rotation: ok %v, started %q; log:\n%s", res.OK(), w.box.started["radarr.service"], w.log.String())
	}
}

func TestARotationFromOpenBaoThatBreaksAConsumerPutsTheOldValueBack(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	w.quadlet("radarr", "radarr-api-key")
	s.put("radarr/api", "api_key", "old")
	w.declare(s, "radarr-api-key radarr/api api_key")
	w.tickWith(s)
	w.box.sick["radarr.service"] = true
	w.box.sickWith["radarr.service"] = "new"
	s.put("radarr/api", "api_key", "new")

	res := w.tickWith(s)

	if res.OK() || w.box.secrets["radarr-api-key"] != "old" || w.box.started["radarr.service"] != "old" {
		t.Fatalf("ok %v, secret %q, started %q; log:\n%s", res.OK(), w.box.secrets["radarr-api-key"], w.box.started["radarr.service"], w.log.String())
	}
}

func TestAStoreThatIsDownKeepsTheValuesAndTheSealsStillApply(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	w.quadlet("radarr", "radarr-api-key")
	s.put("radarr/api", "api_key", "k3y")
	w.declare(s, "radarr-api-key radarr/api api_key")
	w.tickWith(s)
	s.srv.Close()
	w.seal("cloudflare-token", "sealed:t0ken")

	res := w.tickWith(s)

	if res.OK() || !strings.Contains(w.log.String(), "openbao: could not change it") || !strings.Contains(w.log.String(), "cannot be reached") {
		t.Fatalf("a store that is down does not fail the tick, or the log does not say so: ok %v; log:\n%s", res.OK(), w.log.String())
	}
	if w.box.secrets["radarr-api-key"] != "k3y" || w.box.secrets["cloudflare-token"] != "t0ken" {
		t.Fatalf("secrets %v", w.box.secrets)
	}
}

func TestAPathThatCannotBeReadFailsAloneAndTheRestApply(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	s.put("radarr/api", "api_key", "r")
	s.put("sonarr/api", "api_key", "s")
	s.denied["sonarr/api"] = true
	w.declare(s, "radarr-api-key radarr/api api_key", "sonarr-api-key sonarr/api api_key")

	res := w.tickWith(s)

	if res.OK() || w.box.secrets["radarr-api-key"] != "r" || !strings.Contains(w.log.String(), "sonarr-api-key: could not change it") || !strings.Contains(w.log.String(), "permission denied") {
		t.Fatalf("ok %v, secrets %v; log:\n%s", res.OK(), w.box.secrets, w.log.String())
	}
	if _, ok := w.box.secrets["sonarr-api-key"]; ok {
		t.Fatal("a secret that could not be read was loaded")
	}
}

func TestABadLoginFailsAndChangesNothing(t *testing.T) {
	w, s := newWorld(t), newStore(t)
	s.put("radarr/api", "api_key", "r")
	w.declare(s, "radarr-api-key radarr/api api_key")
	w.seal("openbao-approle", `sealed:{"role_id": "role", "secret_id": "wrong"}`)

	res := w.tickWith(s)

	if res.OK() || len(w.box.secrets) != 0 || !strings.Contains(w.log.String(), "invalid role or secret ID") {
		t.Fatalf("ok %v, secrets %v; log:\n%s", res.OK(), w.box.secrets, w.log.String())
	}
}

func TestTheListIsRefusedWhenItIsWrong(t *testing.T) {
	for _, c := range []struct{ why, lines, want string }{
		{"a name also sealed", "cloudflare-token cf/api token", "also sealed"},
		{"a path that climbs", "x ../sys/x value", "not a clean path"},
		{"a name twice", "x a/b v\nx c/d v", "named twice"},
		{"a short line", "x a/b", "is not address"},
	} {
		t.Run(c.why, func(t *testing.T) {
			w, s := newWorld(t), newStore(t)
			w.seal("cloudflare-token", "sealed:t")
			w.declare(s, c.lines)
			if res := w.tickWith(s); res.OK() || !strings.Contains(w.log.String(), c.want) || len(w.box.secrets) != 0 {
				t.Fatalf("ok %v, secrets %v; log:\n%s", res.OK(), w.box.secrets, w.log.String())
			}
		})
	}
}

func TestTheListNeedsAnHTTPSAddressAndASealedLogin(t *testing.T) {
	w := newWorld(t)
	dir := filepath.Join(w.checkout, "nodes", "mutable", "n", "secrets")
	os.MkdirAll(filepath.Join(dir, "sealed"), 0o755)
	src := engine.Source{Checkout: w.checkout, Node: "mutable/n"}
	for _, c := range []struct{ body, want string }{
		{"address http://bao\nlogin l\n", "not https"},
		{"address https://bao\nlogin l\n", "not sealed"},
		{"x a/b v\n", "needs an address line and a login line"},
	} {
		os.WriteFile(filepath.Join(dir, "openbao"), []byte(c.body), 0o644)
		if _, err := CheckBao(src); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%q: %v", c.body, err)
		}
	}
}
