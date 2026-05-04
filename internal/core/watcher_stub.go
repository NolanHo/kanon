//go:build !linux

package core

import (
	"context"
	"errors"
)

type unsupportedWatcher struct{}

func NewWatcher(_ string) (Watcher, error) {
	return &unsupportedWatcher{}, errors.New("inotify watcher is only available on linux")
}

func (w *unsupportedWatcher) Run(ctx context.Context, trigger chan<- WatchChange) error {
	<-ctx.Done()
	return ctx.Err()
}

func (w *unsupportedWatcher) Close() error { return nil }

func (w *unsupportedWatcher) Rebuild() error { return nil }
