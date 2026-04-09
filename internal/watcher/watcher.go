// Package watcher tails Claude Code's JSONL session logs under
// $HOME/.claude/projects and streams parsed events to a handler.
//
// Claude Code writes one JSON object per line per tool call into
// files like:
//
//	~/.claude/projects/<slug>/<session-uuid>.jsonl
//	~/.claude/projects/<slug>/subagents/*.jsonl
//
// The watcher is a thin I/O layer: it uses fsnotify to learn when
// files are created or appended-to, reads new bytes since the last
// known offset, splits them on '\n', and calls the provided
// handler for every parsed billable event.
//
// The watcher is NOT responsible for:
//   - parsing (that lives in internal/parser)
//   - pricing, rollups, budget evaluation, enforcement, push
//     notifications
//
// Those are composed by the CLI's `watch` command, which wires
// parser → pricing → db → budget → enforcer → ntfy into one
// Handler closure and passes it here.
//
// Design notes:
//
//   - Recursive watching: fsnotify watches are single-directory,
//     so we walk the root on startup and add each subdirectory,
//     then add new subdirectories as Create events arrive.
//
//   - Append-tailing: we keep a per-file byte offset. On each Write
//     event, we seek to that offset, read to EOF, and process only
//     complete lines (those ending in '\n'). Incomplete trailing
//     bytes are re-read on the next Write event.
//
//   - Truncation: if a file shrinks below our offset, we reset to
//     0 and reprocess. The db layer dedupes by UUID so no events
//     are double-counted.
//
//   - Handler errors are logged and ignored. One bad event must
//     not stop the watcher. Fatal errors (context cancellation,
//     fsnotify channel close) do return from Run.
//
//   - Dedup across restarts: we do not persist the offsets. On
//     restart we re-scan everything from byte 0. The db layer
//     dedupes by UUID so this is correct but slightly wasteful
//     (O(total-bytes) on every start). Persisting offsets is a
//     v0.2 enhancement — see the TODO in processFile.
package watcher

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/RoninForge/budgetclaw/internal/parser"
)

// Handler is called once per parsed billable event. The source
// argument is the absolute path of the JSONL file the event came
// from — useful for logging and for any future per-file state.
//
// Handler errors are logged by the watcher but do not abort the
// watch loop. If the handler needs to abort (e.g. fatal db
// error), it should cancel the Run context externally.
type Handler func(ctx context.Context, e *parser.Event, source string) error

// Watcher tails a tree of JSONL files and calls a Handler for
// every parsed event. Instantiate with New, then call Run in a
// goroutine (or the main loop) and Close on shutdown.
type Watcher struct {
	root    string
	fs      *fsnotify.Watcher
	handler Handler
	logger  *slog.Logger

	mu    sync.Mutex
	tails map[string]int64 // path → offset of next unread byte
}

// Options tunes a Watcher. All fields are optional.
type Options struct {
	// Logger receives structured debug + error messages. Defaults
	// to a discarding logger (no output) so quiet by default.
	Logger *slog.Logger
}

// New constructs a Watcher rooted at `root`. The directory must
// already exist — that is Claude Code's responsibility, not ours.
// A missing root returns a wrapped fs.ErrNotExist so the CLI can
// print a helpful "run claude code at least once" message.
func New(root string, handler Handler, opts Options) (*Watcher, error) {
	if handler == nil {
		return nil, errors.New("watcher: handler is required")
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("watcher root %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watcher root %s is not a directory", root)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Watcher{
		root:    root,
		fs:      fsw,
		handler: handler,
		logger:  logger,
		tails:   make(map[string]int64),
	}, nil
}

// Close releases the underlying fsnotify resources. Safe to call
// multiple times; subsequent calls are no-ops.
func (w *Watcher) Close() error {
	if w.fs == nil {
		return nil
	}
	err := w.fs.Close()
	w.fs = nil
	return err
}

// Run performs an initial recursive scan of the root directory
// (processing every existing .jsonl file) and then enters a
// blocking event loop. Returns when ctx is canceled or when the
// underlying fsnotify channel closes.
//
// Run may only be called once per Watcher.
func (w *Watcher) Run(ctx context.Context) error {
	if err := w.initialScan(ctx); err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-w.fs.Events:
			if !ok {
				return errors.New("fsnotify events channel closed")
			}
			w.handleFSEvent(ctx, ev)

		case err, ok := <-w.fs.Errors:
			if !ok {
				return errors.New("fsnotify errors channel closed")
			}
			w.logger.Error("fsnotify error", "err", err)
		}
	}
}

// initialScan walks the root directory, registers every
// subdirectory with fsnotify, and processes every existing .jsonl
// file from the top. Non-fatal errors (one unreadable file) are
// logged but do not abort the scan.
func (w *Watcher) initialScan(ctx context.Context) error {
	return filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			w.logger.Warn("walk error", "path", path, "err", err)
			return nil // keep going
		}
		if d.IsDir() {
			if err := w.fs.Add(path); err != nil {
				w.logger.Warn("add watch", "path", path, "err", err)
			}
			return nil
		}
		if isJSONL(path) {
			w.processFile(ctx, path)
		}
		return nil
	})
}

// handleFSEvent dispatches a single fsnotify event to the right
// handler. Create on a directory recursively watches it (and
// processes any .jsonl files already inside, to cover the case
// where mkdir + file-create happen faster than fsnotify can
// deliver separate events). Create or write on a .jsonl file
// processes it. Remove/rename drops any per-file state so the
// offset is not stale if the inode is later reused.
func (w *Watcher) handleFSEvent(ctx context.Context, ev fsnotify.Event) {
	switch {
	case ev.Op&fsnotify.Create != 0:
		info, err := os.Stat(ev.Name)
		if err != nil {
			// File gone before we could stat it; common race with
			// temp files. Not an error.
			return
		}
		if info.IsDir() {
			w.addDirRecursive(ctx, ev.Name)
			return
		}
		if isJSONL(ev.Name) {
			w.processFile(ctx, ev.Name)
		}

	case ev.Op&fsnotify.Write != 0:
		if isJSONL(ev.Name) {
			w.processFile(ctx, ev.Name)
		}

	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		w.mu.Lock()
		delete(w.tails, ev.Name)
		w.mu.Unlock()
	}
}

// addDirRecursive walks `root` and adds every subdirectory to
// the fsnotify watch. Any .jsonl files already present are
// processed immediately. This covers the common race where a
// tool does `mkdir -p a/b/c && echo ... > a/b/c/file.jsonl` and
// the intermediate Create events arrive together.
func (w *Watcher) addDirRecursive(ctx context.Context, root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if err := w.fs.Add(path); err != nil {
				w.logger.Warn("add watch on new dir", "path", path, "err", err)
			}
			return nil
		}
		if isJSONL(path) {
			w.processFile(ctx, path)
		}
		return nil
	})
}

// processFile reads new bytes from a .jsonl file starting at the
// stored offset, parses complete lines, and invokes the handler
// once per billable event. Partial trailing bytes (no '\n' at
// end) are left unconsumed and will be re-read on the next Write.
//
// Error policy:
//   - open / stat / seek / read errors are logged and the call
//     returns. Next Write event will retry.
//   - parser errors on individual lines are logged and skipped.
//   - handler errors are logged and skipped.
//   - nothing here causes Run to exit. Fatal shutdown must come
//     from ctx cancellation.
func (w *Watcher) processFile(ctx context.Context, path string) {
	w.mu.Lock()
	offset := w.tails[path]
	w.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		w.logger.Warn("open file", "path", path, "err", err)
		return
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		w.logger.Warn("stat file", "path", path, "err", err)
		return
	}

	// Truncation detection: if the file shrunk below our offset,
	// the user rotated or truncated it. Reset and reprocess.
	if stat.Size() < offset {
		w.logger.Info("file truncated, resetting offset",
			"path", path, "old_offset", offset, "new_size", stat.Size())
		offset = 0
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		w.logger.Warn("seek file", "path", path, "err", err)
		return
	}

	buf, err := io.ReadAll(f)
	if err != nil {
		w.logger.Warn("read file", "path", path, "err", err)
		return
	}

	// Find the last complete line. Everything up to and including
	// the last '\n' is complete; everything after is a partial.
	// On a file that has never had a newline yet, we do nothing
	// and leave the offset untouched.
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		return
	}
	complete := buf[:lastNL+1]

	// Use bufio.Scanner on the complete region so each Parse call
	// receives exactly one line without trailing '\n' noise.
	scanner := bufio.NewScanner(bytes.NewReader(complete))
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, err := parser.Parse(line)
		if err != nil {
			w.logger.Warn("parse error", "path", path, "err", err)
			continue
		}
		if ev == nil {
			continue // non-billable line type
		}
		if err := w.handler(ctx, ev, path); err != nil {
			w.logger.Error("handler error",
				"path", path, "uuid", ev.UUID, "err", err)
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		w.logger.Warn("scanner error", "path", path, "err", err)
		// Fall through: any successful events above are already
		// processed and we advance the offset past them anyway.
	}

	// Advance the offset past the last complete line.
	newOffset := offset + int64(lastNL+1)
	w.mu.Lock()
	w.tails[path] = newOffset
	w.mu.Unlock()
}

// isJSONL returns true if path has a .jsonl suffix (case-sensitive).
// Claude Code always writes .jsonl; we don't need to accept .JSONL.
func isJSONL(path string) bool {
	return strings.HasSuffix(path, ".jsonl")
}
