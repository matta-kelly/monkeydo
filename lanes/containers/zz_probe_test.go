package containers

import (
	"os"
	"testing"
)

func TestZZProbe(t *testing.T) {
	w := accountAppWorld(t)
	w.appTick("assembly")
	os.RemoveAll(w.checkout + "/nodes/mutable/n/users/assembly/apps")
	res := w.appTick("assembly")
	t.Logf("res %v\nlog:%s", res, w.log.String())
	t.Logf("drop %v", at(w, "shop.container.d/node.conf"))
}

func at(w *acctWorld, f string) bool {
	_, ok := w.at("assembly", f)
	return ok
}
