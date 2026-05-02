package bridge

import "context"

type WatchChange struct {
	Path  string
	IsDir bool
	Full  bool
}

type Watcher interface {
	Run(ctx context.Context, trigger chan<- WatchChange) error
	Close() error
	Rebuild() error
}
