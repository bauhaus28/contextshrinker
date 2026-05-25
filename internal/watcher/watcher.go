package watcher

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"contextshrinker/internal/ignore"
)

type Watcher struct {
	fsWatcher  *fsnotify.Watcher
	rootDir    string
	ignoreList *ignore.IgnoreList
	callback   func(string)
	timers     map[string]*time.Timer
	timersMu   sync.Mutex
	done       chan struct{}
	closeOnce  sync.Once
}

// NewWatcher creates a new fsnotify file watcher.
func NewWatcher(rootDir string, ignoreList *ignore.IgnoreList, callback func(string)) (*Watcher, error) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		fsWatcher:  fsWatcher,
		rootDir:    rootDir,
		ignoreList: ignoreList,
		callback:   callback,
		timers:     make(map[string]*time.Timer),
		done:       make(chan struct{}),
	}, nil
}

// Start walking the workspace root, registers directories, and enters the watch loop.
func (w *Watcher) Start() error {
	// Walk and add directories
	err := filepath.WalkDir(w.rootDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(w.rootDir, path)
		if err != nil {
			return nil
		}

		if rel != "." && w.ignoreList.ShouldIgnore(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			err = w.fsWatcher.Add(path)
			if err != nil {
				log.Printf("Failed to watch directory %s: %v", path, err)
			}
		}
		return nil
	})
	if err != nil {
		w.fsWatcher.Close()
		return err
	}

	go w.watchLoop()
	return nil
}

func (w *Watcher) Close() {
	w.closeOnce.Do(func() {
		close(w.done)
		w.fsWatcher.Close()

		w.timersMu.Lock()
		defer w.timersMu.Unlock()
		for _, t := range w.timers {
			t.Stop()
		}
	})
}

func (w *Watcher) watchLoop() {
	for {
		select {
		case <-w.done:
			return
		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}

			rel, err := filepath.Rel(w.rootDir, event.Name)
			if err != nil {
				continue
			}

			if w.ignoreList.ShouldIgnore(rel) {
				continue
			}

			// If a new directory is created, watch it dynamically
			if event.Has(fsnotify.Create) {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					err = w.fsWatcher.Add(event.Name)
					if err == nil {
						log.Printf("Dynamically watching directory: %s", event.Name)
					}
				}
			}

			// We are only interested in modifications or creation/renames of source files
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				// Filter for valid code files based on extension
				ext := strings.ToLower(filepath.Ext(event.Name))
				if ext == ".go" || ext == ".py" || ext == ".js" || ext == ".jsx" || ext == ".ts" || ext == ".tsx" || ext == ".java" {
					w.debounce(event.Name)
				}
			}
		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			log.Printf("Watcher error: %v", err)
		}
	}
}

func (w *Watcher) debounce(filePath string) {
	w.timersMu.Lock()
	defer w.timersMu.Unlock()

	if timer, exists := w.timers[filePath]; exists {
		timer.Stop()
	}

	w.timers[filePath] = time.AfterFunc(1500*time.Millisecond, func() {
		w.timersMu.Lock()
		delete(w.timers, filePath)
		w.timersMu.Unlock()

		log.Printf("File changed and debounced: %s", filePath)
		w.callback(filePath)
	})
}

