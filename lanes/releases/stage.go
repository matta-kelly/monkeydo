package releases

import (
	"context"
	"fmt"
	"net/http"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// Stage installs each release that names lists under n's root, as the lane installs it on a node: fetched by its pin,
// proved by its digest, and read back. It is for monkeydo check, which renders what a node will install from the
// commit alone, never from what is on the machine it runs on. The registry login is n's auth file, as on a node; a
// name with no pin is left out, for the check of the pins to name.
func Stage(ctx context.Context, src engine.Source, n node.Node, client *http.Client, names []string) error {
	rels, err := Read(src)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	l := Lane{Node: n, HTTP: client}.paths()
	for _, r := range rels {
		if !want[r.Name] {
			continue
		}
		c := &change{l: l, r: r, keep: true}
		if _, err := c.Fix(ctx); err != nil {
			return fmt.Errorf("%s %s could not be fetched from its pin: %v", r.Name, r.Version, err)
		}
		if err := c.Healthy(ctx); err != nil {
			return fmt.Errorf("%s: %v", r.Name, err)
		}
	}
	return nil
}
