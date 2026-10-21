package config

import (
	"bytes"
	"context"
	"os"
	"time"
)

// Watcher reloads a config file when its contents change.
//
// It polls rather than subscribing to filesystem events. Kubernetes updates
// a mounted ConfigMap by atomically swapping a symlink, which a watch on the
// file itself does not see, and the kubelet takes up to a minute to
// propagate a change anyway, so polling every few seconds adds no
// noticeable delay. Comparing contents rather than modification times means
// a change is detected however the file was replaced.
type Watcher struct {
	path    string
	last    []byte // the contents last read, valid or not
	lastErr string // the last read error, so a missing file is reported once
}

// NewWatcher loads the config at path and returns it with a Watcher for
// later changes.
func NewWatcher(path string) (*Watcher, *Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator, not by untrusted input
	if err != nil {
		return nil, nil, err
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	return &Watcher{path: path, last: data}, cfg, nil
}

// Run checks the file every interval, and immediately whenever trigger
// receives, until ctx is done. When the contents have changed, it calls
// apply with the new config, or reject with the reason they are invalid, in
// which case the previous config stays in effect. Each distinct problem is
// reported once, not on every check.
func (w *Watcher) Run(ctx context.Context, interval time.Duration, trigger <-chan os.Signal, apply func(*Config), reject func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
		}
		w.check(apply, reject)
	}
}

func (w *Watcher) check(apply func(*Config), reject func(error)) {
	data, err := os.ReadFile(w.path)
	if err != nil {
		if err.Error() != w.lastErr {
			w.lastErr = err.Error()
			reject(err)
		}
		return
	}
	w.lastErr = ""
	if bytes.Equal(data, w.last) {
		return
	}
	w.last = data
	cfg, err := Parse(data)
	if err != nil {
		reject(err)
		return
	}
	apply(cfg)
}
