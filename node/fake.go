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
	Do func(line string) (out string, err error, handled bool)

	mu    sync.Mutex
	calls []string
}

func (f *Fake) Path(p string) string { return filepath.Join(f.Root, p) }

func (f *Fake) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, line)
	f.mu.Unlock()
	if f.Do != nil {
		if out, err, ok := f.Do(line); ok {
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
