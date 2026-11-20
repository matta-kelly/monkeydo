package secrets

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// A secret can also come from OpenBao, the fleet's store for machine secrets. git lists each in
// nodes/<node>/secrets/openbao: the store's address, the sealed AppRole login the node uses, and one line for each
// podman secret, with the path it is read from under the KV store `secret/` and the field it is:
//
//	address https://openbao.example
//	login   openbao-approle
//	radarr-api-key  radarr/api  api_key
//
// The login is a sealed secret of this node, {"role_id": "...", "secret_id": "..."}: secret zero, as the node needs it
// before it can reach the store. It is used only to log in, never loaded as a podman secret. A name comes from
// OpenBao or from a seal, never both.
//
// Each tick logs in, reads every path, and compares each value with the podman secret: one that differs is loaded and
// its consumers restarted, as a sealed one is. A store that cannot be reached, or a path that cannot be read, fails as
// one change and leaves the podman secrets as they are: a node keeps running on the values it has while the store is
// down. The token is revoked at the end of the plan; the values are in hand by then.

var baoPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

// baoEntry is one podman secret read from OpenBao.
type baoEntry struct {
	name  string // the podman secret
	path  string // under secret/
	field string // the key in that secret
}

// baoDecl is a node's secrets/openbao.
type baoDecl struct {
	addr    string
	login   string // the sealed name of the AppRole login
	entries []baoEntry
}

// Bao is the node's OpenBao list in git, checked: an address, a login sealed for this node, and each name once, sealed
// nowhere. A missing list is none.
func Bao(src engine.Source) (*baoDecl, error) {
	dir := filepath.Join(src.Checkout, "nodes", src.Node, "secrets")
	f, err := os.Open(filepath.Join(dir, "openbao"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sealed := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, "sealed", name+".sops"))
		return err == nil
	}
	d := &baoDecl{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
			continue
		case len(fields) == 2 && fields[0] == "address":
			if !strings.HasPrefix(fields[1], "https://") {
				return nil, fmt.Errorf("secrets/openbao: the address %s is not https://", fields[1])
			}
			d.addr = strings.TrimSuffix(fields[1], "/")
		case len(fields) == 2 && fields[0] == "login":
			if !secretName.MatchString(fields[1]) || !sealed(fields[1]) {
				return nil, fmt.Errorf("secrets/openbao: the login %s is not sealed for this node", fields[1])
			}
			d.login = fields[1]
		case len(fields) == 3:
			e := baoEntry{name: fields[0], path: fields[1], field: fields[2]}
			if !secretName.MatchString(e.name) {
				return nil, fmt.Errorf("secrets/openbao: %q is not a secret name", e.name)
			}
			if !baoPath.MatchString(e.path) || strings.Contains("/"+e.path+"/", "/../") || strings.Contains("/"+e.path+"/", "/./") {
				return nil, fmt.Errorf("secrets/openbao: %s is not a clean path under secret/", e.path)
			}
			if sealed(e.name) {
				return nil, fmt.Errorf("secrets/openbao: %s is also sealed; a secret comes from one place", e.name)
			}
			if seen[e.name] {
				return nil, fmt.Errorf("secrets/openbao: %s is named twice", e.name)
			}
			seen[e.name] = true
			d.entries = append(d.entries, e)
		default:
			return nil, fmt.Errorf("secrets/openbao: %q is not address <url>, login <sealed name> or <name> <path> <field>", strings.TrimSpace(line))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if d.addr == "" || d.login == "" {
		return nil, fmt.Errorf("secrets/openbao: needs an address line and a login line")
	}
	return d, nil
}

// CheckBao proves the list with no node, for CI.
func CheckBao(src engine.Source) (int, error) {
	d, err := Bao(src)
	if d == nil {
		return 0, err
	}
	return len(d.entries), err
}

// bao is a client of one OpenBao, logged in with a token. Its errors say what failed, never a value or the token.
type bao struct {
	addr   string
	token  string
	client *http.Client
}

func (b *bao) call(ctx context.Context, method, path string, body any) (map[string]any, error) {
	var in io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		in = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.addr+"/v1/"+path, in)
	if err != nil {
		return nil, err
	}
	if b.token != "" {
		req.Header.Set("X-Vault-Token", b.token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		// the error holds the URL, which holds no secret
		return nil, fmt.Errorf("openbao %s: cannot be reached: %v", path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("openbao %s: %s, not JSON", path, resp.Status)
		}
	}
	if resp.StatusCode >= 300 {
		// OpenBao's errors name the problem ("permission denied"), never a value
		return nil, fmt.Errorf("openbao %s: %s %v", path, resp.Status, out["errors"])
	}
	return out, nil
}

// login trades the AppRole login for a token.
func (b *bao) login(ctx context.Context, cred []byte) error {
	var c struct {
		RoleID   string `json:"role_id"`
		SecretID string `json:"secret_id"`
	}
	if err := json.Unmarshal(cred, &c); err != nil || c.RoleID == "" || c.SecretID == "" {
		return fmt.Errorf("the login is not {\"role_id\": ..., \"secret_id\": ...}")
	}
	out, err := b.call(ctx, http.MethodPost, "auth/approle/login", map[string]string{"role_id": c.RoleID, "secret_id": c.SecretID})
	if err != nil {
		return err
	}
	auth, _ := out["auth"].(map[string]any)
	token, _ := auth["client_token"].(string)
	if token == "" {
		return fmt.Errorf("openbao auth/approle/login: no token in the answer")
	}
	b.token = token
	return nil
}

// read is one field of the KV secret at path: a string, as podman takes it.
func (b *bao) read(ctx context.Context, path, field string) ([]byte, error) {
	out, err := b.call(ctx, http.MethodGet, "secret/data/"+path, nil)
	if err != nil {
		return nil, err
	}
	data, _ := out["data"].(map[string]any)
	fields, _ := data["data"].(map[string]any)
	v, ok := fields[field].(string)
	if !ok {
		return nil, fmt.Errorf("openbao secret/%s has no string field %s", path, field)
	}
	return []byte(v), nil
}

// revoke ends the token. A token that outlives it expires on its own, so a failure is ignored.
func (b *bao) revoke(ctx context.Context) {
	if b.token != "" {
		b.call(ctx, http.MethodPost, "auth/token/revoke-self", nil)
		b.token = ""
	}
}

// planBao reads each listed secret from OpenBao and names each podman secret that differs. cred is the opened login.
func (l Lane) planBao(ctx context.Context, d *baoDecl, cred []byte) []engine.Change {
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	b := &bao{addr: d.addr, client: client}
	if err := b.login(ctx, cred); err != nil {
		return []engine.Change{&baoFailed{thing: "openbao", err: err}}
	}
	defer b.revoke(ctx)
	var changes []engine.Change
	for _, e := range d.entries {
		value, err := b.read(ctx, e.path, e.field)
		if err != nil {
			changes = append(changes, &baoFailed{thing: e.name, err: err})
			continue
		}
		have, ok := l.current(ctx, e.name)
		if ok && bytes.Equal(have, value) {
			continue
		}
		changes = append(changes, &baoChange{l: l, name: e.name, value: value})
	}
	return changes
}

// current is the value of the podman secret name. ok false: there is none.
func (l Lane) current(ctx context.Context, name string) ([]byte, bool) {
	out, err := l.Node.RunCmd(ctx, node.Cmd{Name: "podman", Args: []string{"secret", "inspect", "--showsecret", "--format", "{{.SecretData}}", name}, Quiet: true, Raw: true})
	if err != nil {
		return nil, false
	}
	// --format ends its output with a newline that is not the value's
	return []byte(strings.TrimSuffix(out, "\n")), true
}

// baoChange loads a value read from OpenBao as the podman secret name, and restarts its consumers.
type baoChange struct {
	l     Lane
	name  string
	value []byte
}

func (c *baoChange) Thing() string { return c.name }
func (c *baoChange) Owned() bool   { return true }

func (c *baoChange) Fix(ctx context.Context) (engine.Undo, error) {
	before, had := c.l.current(ctx, c.name)
	undo := func(ctx context.Context) error {
		if had {
			if err := c.l.load(ctx, c.name, before); err != nil {
				return err
			}
		} else {
			c.l.Node.Run(ctx, "podman", "secret", "rm", c.name)
		}
		return c.l.restartConsumers(ctx, c.name)
	}
	return undo, c.l.load(ctx, c.name, c.value)
}

func (c *baoChange) Healthy(ctx context.Context) error { return c.l.settleConsumers(ctx, c.name) }

// baoFailed is a secret the tick could not read from OpenBao: it fails, and the podman secret stays as it is.
type baoFailed struct {
	thing string
	err   error
}

func (c *baoFailed) Thing() string                            { return c.thing }
func (c *baoFailed) Owned() bool                              { return true }
func (c *baoFailed) Fix(context.Context) (engine.Undo, error) { return nil, c.err }
func (c *baoFailed) Healthy(context.Context) error            { return nil }
