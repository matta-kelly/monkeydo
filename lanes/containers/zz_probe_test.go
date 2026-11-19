package containers

import (
	"context"
	"testing"
)

func TestZZProbe(t *testing.T) {
	w := accountAppWorld(t)
	sc, _ := (Lane{Node: w.box.n, Account: "assembly"}).scope(context.Background())
	sc.write(context.Background(), "shop.container", []byte("x"))
	got, err := sc.list(context.Background())
	t.Logf("list %q %v", got, err)
	for _, c := range w.box.n.Calls() {
		t.Logf("call %q", c)
	}
}
