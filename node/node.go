// Package node is the only code that touches the machine: its files, under a root, and the commands it runs. A lane
// asks the node; a test gives it a fake one.
package node

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Node is a machine: files under Root, and commands.
type Node interface {
	// Path is p, an absolute path on the machine, under the node's root.
	Path(p string) string
	// Run runs a command and returns its output, stdout and stderr together. A command that fails, or passes its time
	// limit, is an error that names it and holds its output.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Real is the machine this runs on. Root is "/" on a node, or a folder that stands for it.
type Real struct {
	Root  string
	Limit time.Duration // each command's time limit; 0 means 60 seconds
}

func (r Real) Path(p string) string { return filepath.Join(r.Root, p) }

func (r Real) Run(ctx context.Context, name string, args ...string) (string, error) {
	limit := r.Limit
	if limit == 0 {
		limit = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("%s passed its limit of %s", strings.Join(append([]string{name}, args...), " "), limit)
	}
	if err != nil {
		return text, fmt.Errorf("%s: %v: %s", strings.Join(append([]string{name}, args...), " "), err, text)
	}
	return text, nil
}

// File is a regular file's content and mode.
type File struct {
	Data []byte
	Mode os.FileMode
}

// Read reads the file at p on the node. A missing file is (File{}, false, nil).
func Read(n Node, p string) (File, bool, error) {
	full := n.Path(p)
	info, err := os.Lstat(full)
	if os.IsNotExist(err) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	if !info.Mode().IsRegular() {
		return File{}, false, fmt.Errorf("%s is not a regular file", p)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return File{}, false, err
	}
	return File{Data: data, Mode: info.Mode().Perm()}, true, nil
}

// Write puts f at p on the node atomically: a temporary file in the same folder, synced, then renamed over p. The
// folders it needs are made with mode 0755.
func Write(n Node, p string, f File) error {
	full := n.Path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".monkeydo-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(f.Data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, f.Mode); err != nil {
		return err
	}
	return os.Rename(name, full)
}

// Remove removes the file at p on the node. A missing file is no error.
func Remove(n Node, p string) error {
	err := os.Remove(n.Path(p))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
