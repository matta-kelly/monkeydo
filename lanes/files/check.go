package files

import (
	"path/filepath"

	"github.com/matta-kelly/monkeydo/engine"
)

// Check proves a node's trees with no node at all: each file regular, and none a path monkeydo may not own. CI runs
// it, so a tree it passes is one the lane will not refuse.
func Check(src engine.Source) (int, error) {
	want, err := read(filepath.Join(src.Checkout, "nodes", "_shared", "etc"), filepath.Join(src.Checkout, "nodes", src.Node, "etc"))
	return len(want), err
}
