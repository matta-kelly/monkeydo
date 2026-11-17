package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// Fake is a node for tests: real files under Root, and commands that are written down and answered from Answers.
type Fake struct {
	Root string
	// Answers maps a command line ("sysctl -n vm.dirty_bytes") to its output; a line in Fails fails with that text.
	Answers map[string]string
	Fails   map[string]string
	// Do, when set, answers a command first: handled false hands it on to Fails and Answers. A test uses it for a
	// machine whose answers change, such as dpkg after an install.
	Do func(line string, stdin []byte) (out string, err error, handled bool)

	mu    sync.Mutex
	calls []string
}

func (f *Fake) Path(p string) string { return filepath.Join(f.Root, p) }

func (f *Fake) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f.RunCmd(ctx, Cmd{Name: name, Args: args})
}

// RunCmd writes the command line down, never its stdin.
func (f *Fake) RunCmd(_ context.Context, c Cmd) (string, error) {
	line := c.line()
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	if f.Do != nil {
		if out, err, ok := f.Do(line, c.Stdin); ok {
			// as Real does: output is trimmed unless the command asks for it raw
			if !c.Raw {
				out = strings.TrimSpace(out)
			}
			return out, err
		}
	}
	if msg, ok := f.Fails[line]; ok {
		return msg, fmt.Errorf("%s: %s", line, msg)
	}
	return f.Answers[line], nil
}

// Calls is every command run, in order.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
