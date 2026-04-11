package bridge

import "context"

type Watcher interface {
	Run(ctx context.Context, trigger chan<- struct{}) error
	Close() error
	Rebuild() error
}
