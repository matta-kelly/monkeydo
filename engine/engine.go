// Package engine is monkeydo's loop: each lane compares git with the node and names its differences, and the engine
// takes each difference through fix, then health, and puts back the one that fails. A difference is the unit of
// rollback: one bad change never undoes the others.
package engine

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// Source is what a tick reads: the declaration's checkout, and this node's folder in it.
type Source struct {
	Checkout string // the dazoo checkout, e.g. /var/lib/dazoo/repo
	Node     string // this node's folder under nodes/, e.g. mutable/monkeybusiness
}

// Undo puts a change back: the thing as it was, live again.
type Undo func(ctx context.Context) error

// A Change is one difference between git and the node.
type Change interface {
	// Thing names what it changes, as the log says it: a path, a package, an account.
	Thing() string
	// Fix makes the thing match git, and returns how to put it back.
	Fix(ctx context.Context) (Undo, error)
	// Healthy says whether the change took: the reload passed, the value reads back, the unit stays up.
	Healthy(ctx context.Context) error
	// Owned says whether the lane owns the thing once the change holds: true for a thing git has, false for one it
	// removes.
	Owned() bool
}

// A Lane is one kind of thing a node has.
type Lane interface {
	Name() string
	// Plan reads git and the node, and returns one Change per difference, and every thing git has for this lane. owned
	// is what the lane owned after the last tick, so a thing that left git can be removed. A Plan that cannot read or
	// prove git returns an error, and the lane changes nothing this tick.
	Plan(ctx context.Context, src Source, owned []string) (changes []Change, want []string, err error)
}

// Result is what a tick did: each lane's things that failed. An empty result is a node true to git.
type Result struct {
	Failed map[string][]string // lane -> things that failed, or the lane itself when its Plan failed
}

// OK is whether every lane ran and every change held.
func (r Result) OK() bool { return len(r.Failed) == 0 }

// Run takes each lane through one tick, in order, and keeps rec up to date: what each lane owns after it.
func Run(ctx context.Context, src Source, lanes []Lane, rec *Record, log io.Writer) Result {
	res := Result{Failed: map[string][]string{}}
	for _, lane := range lanes {
		name := lane.Name()
		prev := rec.Owned(name)
		changes, want, err := lane.Plan(ctx, src, prev)
		if err != nil {
			fmt.Fprintf(log, "monkeydo: %s: refused, nothing changed: %v\n", name, err)
			res.Failed[name] = append(res.Failed[name], name)
			continue
		}
		owned := set(want)
		was := set(prev)
		for _, c := range changes {
			if err := apply(ctx, name, c, log); err != nil {
				res.Failed[name] = append(res.Failed[name], c.Thing())
				// the thing stays as it was, so its ownership does too
				if was[c.Thing()] {
					owned[c.Thing()] = true
				} else {
					delete(owned, c.Thing())
				}
				continue
			}
			if c.Owned() {
				owned[c.Thing()] = true
			} else {
				delete(owned, c.Thing())
			}
		}
		rec.Set(name, keys(owned))
	}
	return res
}

// apply takes one change through fix and health, and puts it back when health fails.
func apply(ctx context.Context, lane string, c Change, log io.Writer) error {
	undo, err := c.Fix(ctx)
	if err != nil {
		fmt.Fprintf(log, "monkeydo: %s: %s: could not change it, left as it was: %v\n", lane, c.Thing(), err)
		if undo != nil {
			if uerr := undo(ctx); uerr != nil {
				fmt.Fprintf(log, "monkeydo: %s: %s: COULD NOT ROLL BACK: %v\n", lane, c.Thing(), uerr)
			}
		}
		return err
	}
	if err := c.Healthy(ctx); err != nil {
		fmt.Fprintf(log, "monkeydo: %s: %s: unhealthy, rolling back: %v\n", lane, c.Thing(), err)
		if uerr := undo(ctx); uerr != nil {
			fmt.Fprintf(log, "monkeydo: %s: %s: COULD NOT ROLL BACK: %v\n", lane, c.Thing(), uerr)
		} else {
			fmt.Fprintf(log, "monkeydo: %s: %s: rolled back\n", lane, c.Thing())
		}
		return err
	}
	fmt.Fprintf(log, "monkeydo: %s: %s: ok\n", lane, c.Thing())
	return nil
}

func set(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
