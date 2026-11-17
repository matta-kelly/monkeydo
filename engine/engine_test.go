package engine

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeChange records what the engine did with it.
type fakeChange struct {
	thing      string
	owned      bool
	fixErr     error
	healthyErr error
	fixed      bool
	undone     bool
}

func (c *fakeChange) Thing() string { return c.thing }
func (c *fakeChange) Owned() bool   { return c.owned }
func (c *fakeChange) Fix(context.Context) (Undo, error) {
	c.fixed = true
	return func(context.Context) error { c.undone = true; return nil }, c.fixErr
}
func (c *fakeChange) Healthy(context.Context) error { return c.healthyErr }

type fakeLane struct {
	name    string
	changes []*fakeChange
	want    []string
	err     error
	gotOwn  []string
}

func (l *fakeLane) Name() string { return l.name }
func (l *fakeLane) Plan(_ context.Context, _ Source, owned []string) ([]Change, []string, error) {
	l.gotOwn = owned
	if l.err != nil {
		return nil, nil, l.err
	}
	out := make([]Change, len(l.changes))
	for i, c := range l.changes {
		out[i] = c
	}
	return out, l.want, nil
}

func newRecord(t *testing.T) *Record {
	t.Helper()
	r, err := LoadRecord(filepath.Join(t.TempDir(), "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAnUnhealthyChangeIsRolledBackAloneAndTheRestHold(t *testing.T) {
	good := &fakeChange{thing: "/etc/a", owned: true}
	bad := &fakeChange{thing: "/etc/b", owned: true, healthyErr: errors.New("value did not read back")}
	after := &fakeChange{thing: "/etc/c", owned: true}
	lane := &fakeLane{name: "files", changes: []*fakeChange{good, bad, after}, want: []string{"/etc/a", "/etc/b", "/etc/c"}}
	rec := newRecord(t)
	var log bytes.Buffer

	res := Run(context.Background(), Source{}, []Lane{lane}, rec, &log)

	if good.undone || after.undone || !bad.undone {
		t.Fatalf("undone: good %v, bad %v, after %v; want only bad", good.undone, bad.undone, after.undone)
	}
	if !after.fixed {
		t.Fatal("the change after the failed one was not applied")
	}
	if got := res.Failed["files"]; !reflect.DeepEqual(got, []string{"/etc/b"}) {
		t.Fatalf("failed = %v, want [/etc/b]", got)
	}
	if got := rec.Owned("files"); !reflect.DeepEqual(got, []string{"/etc/a", "/etc/c"}) {
		t.Fatalf("owned = %v; a failed new thing is not owned", got)
	}
	if !strings.Contains(log.String(), "/etc/b: rolled back") {
		t.Fatalf("log does not say the rollback:\n%s", log.String())
	}
}

func TestAChangeThatCannotBeMadeIsLeftAsItWas(t *testing.T) {
	c := &fakeChange{thing: "/etc/a", owned: true, fixErr: errors.New("read-only")}
	lane := &fakeLane{name: "files", changes: []*fakeChange{c}, want: []string{"/etc/a"}}
	res := Run(context.Background(), Source{}, []Lane{lane}, newRecord(t), &bytes.Buffer{})
	if res.OK() || !c.undone {
		t.Fatalf("ok %v, undone %v; want failed and undone", res.OK(), c.undone)
	}
}

func TestARemovalDropsOwnershipAndAFailedOneKeepsIt(t *testing.T) {
	rec := newRecord(t)
	rec.Set("files", []string{"/etc/gone", "/etc/stuck"})
	gone := &fakeChange{thing: "/etc/gone", owned: false}
	stuck := &fakeChange{thing: "/etc/stuck", owned: false, healthyErr: errors.New("reload failed")}
	lane := &fakeLane{name: "files", changes: []*fakeChange{gone, stuck}}

	Run(context.Background(), Source{}, []Lane{lane}, rec, &bytes.Buffer{})

	if !reflect.DeepEqual(lane.gotOwn, []string{"/etc/gone", "/etc/stuck"}) {
		t.Fatalf("the lane was given %v, not what it owned", lane.gotOwn)
	}
	if got := rec.Owned("files"); !reflect.DeepEqual(got, []string{"/etc/stuck"}) {
		t.Fatalf("owned = %v; want the failed removal still owned", got)
	}
}

func TestARefusedLaneChangesNothingAndTheNextLaneRuns(t *testing.T) {
	rec := newRecord(t)
	rec.Set("files", []string{"/etc/a"})
	refused := &fakeLane{name: "files", err: errors.New("a file names a path monkeydo may not own")}
	next := &fakeLane{name: "packages", changes: []*fakeChange{{thing: "catatonit", owned: true}}, want: []string{"catatonit"}}
	var log bytes.Buffer

	res := Run(context.Background(), Source{}, []Lane{refused, next}, rec, &log)

	if !reflect.DeepEqual(rec.Owned("files"), []string{"/etc/a"}) {
		t.Fatalf("a refused lane changed its record: %v", rec.Owned("files"))
	}
	if !reflect.DeepEqual(rec.Owned("packages"), []string{"catatonit"}) {
		t.Fatalf("the next lane did not run: %v", rec.Owned("packages"))
	}
	if !reflect.DeepEqual(res.Failed["files"], []string{"files"}) || !strings.Contains(log.String(), "files: refused") {
		t.Fatalf("failed %v, log:\n%s", res.Failed, log.String())
	}
}

func TestATickWithNoDifferenceChangesNothingAndSaysNothing(t *testing.T) {
	rec := newRecord(t)
	lane := &fakeLane{name: "files", want: []string{"/etc/a"}}
	var log bytes.Buffer
	res := Run(context.Background(), Source{}, []Lane{lane}, rec, &log)
	if !res.OK() || log.Len() != 0 {
		t.Fatalf("ok %v, log %q; want ok and silent", res.OK(), log.String())
	}
	if !reflect.DeepEqual(rec.Owned("files"), []string{"/etc/a"}) {
		t.Fatalf("owned = %v; what git has and the node matches is owned", rec.Owned("files"))
	}
}

func TestTheRecordSurvivesASaveAndALoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "record.json")
	r, _ := LoadRecord(path)
	r.Set("files", []string{"/etc/b", "/etc/a"})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := LoadRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Owned("files"), []string{"/etc/a", "/etc/b"}) {
		t.Fatalf("owned after a load = %v", again.Owned("files"))
	}
}
