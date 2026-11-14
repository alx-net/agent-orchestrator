// Package diagnostics writes goroutine dumps and watches for a lock that stops
// moving, so a daemon that wedges says so and leaves evidence on disk instead of
// freezing silently. It imports only the standard library.
package diagnostics

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DirName is the data-dir subdirectory dumps are written to.
	DirName    = "diagnostics"
	dumpPrefix = "goroutines-"
	dumpSuffix = ".txt"
	// defaultKeep is how many dumps survive pruning. A dump of a busy daemon is
	// a few MB; a handful is enough to compare a stall with what came before it.
	defaultKeep = 5
)

// Dumper writes full goroutine dumps (the same stacks SIGQUIT prints, without
// killing the process) under <dataDir>/diagnostics and keeps only the newest few.
type Dumper struct {
	dir  string
	keep int
	now  func() time.Time

	mu sync.Mutex // one dump and prune at a time
}

// NewDumper writes under dataDir's diagnostics directory.
func NewDumper(dataDir string) *Dumper {
	return &Dumper{dir: filepath.Join(dataDir, DirName), keep: defaultKeep, now: time.Now}
}

// Dir is where dumps are written.
func (d *Dumper) Dir() string { return d.dir }

// Dump writes every goroutine's stack, headed by reason, to a new file and
// returns its path. Older dumps beyond the newest few are removed.
func (d *Dumper) Dump(reason string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		return "", fmt.Errorf("create diagnostics dir: %w", err)
	}
	at := d.now().UTC()
	path := filepath.Join(d.dir, dumpPrefix+at.Format("20060102T150405.000000000Z")+dumpSuffix)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create goroutine dump: %w", err)
	}
	_, err = fmt.Fprintf(f, "reason: %s\ntime: %s\npid: %d\n\n", reason, at.Format(time.RFC3339Nano), os.Getpid())
	if err == nil {
		// debug=2 is the panic/SIGQUIT format: every goroutine, full stacks,
		// with how long each has been blocked.
		err = pprof.Lookup("goroutine").WriteTo(f, 2)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return path, fmt.Errorf("write goroutine dump: %w", err)
	}
	d.prune()
	return path, nil
}

// prune removes all but the newest keep dumps. Names sort by time.
func (d *Dumper) prune() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	var dumps []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasPrefix(name, dumpPrefix) && strings.HasSuffix(name, dumpSuffix) {
			dumps = append(dumps, name)
		}
	}
	if len(dumps) <= d.keep {
		return
	}
	sort.Strings(dumps)
	for _, name := range dumps[:len(dumps)-d.keep] {
		_ = os.Remove(filepath.Join(d.dir, name))
	}
}
