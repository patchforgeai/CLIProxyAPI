package watcher

import "os"

// devyreAuthFileExists reports whether an auth file is there: a remove event that comes right
// after another one is a duplicate only then (handleEvent's debounce).
func devyreAuthFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
