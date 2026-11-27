package releases

import (
	"context"
	"fmt"

	"github.com/matta-kelly/monkeydo/engine"
	"github.com/matta-kelly/monkeydo/node"
)

// For monkeydo check, which renders what a node will install from the commit alone. The check holds no credential and
// reaches no registry: the releases it renders from are fetched before it runs, by what Pins names, into an OCI image
// layout, and Stage installs them from there, each blob proved by its digest as on a node.

// Pins are the releases that names lists, as git pins them: what a check of the node must have fetched.
func Pins(src engine.Source, names []string) ([]Release, error) {
	rels, err := Read(src)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	var out []Release
	for _, r := range rels {
		if want[r.Name] {
			out = append(out, r)
		}
	}
	return out, nil
}

// Stage installs each release that names lists under n's root, as the lane installs it on a node, from blobs, an OCI
// image layout of fetched blobs, and reads each back. A name with no pin is left out, for the check of the pins to
// name.
func Stage(ctx context.Context, src engine.Source, n node.Node, blobs string, names []string) error {
	pins, err := Pins(src, names)
	if err != nil || len(pins) == 0 {
		return err
	}
	if blobs == "" {
		return fmt.Errorf("the node's apps run from %d pinned releases, and no fetched releases were given (--releases)", len(pins))
	}
	l := Lane{Node: n, Blobs: blobs}.paths()
	for _, r := range pins {
		c := &change{l: l, r: r, keep: true}
		if _, err := c.Fix(ctx); err != nil {
			return fmt.Errorf("%s %s: %v", r.Name, r.Version, err)
		}
		if err := c.Healthy(ctx); err != nil {
			return fmt.Errorf("%s: %v", r.Name, err)
		}
	}
	return nil
}
