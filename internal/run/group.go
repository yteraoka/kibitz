// Package run coordinates the long-running components of a binary: the first
// one to stop cancels the rest, and Run reports the first non-nil error.
package run

import (
	"context"
	"sync"
)

// Func is a component that runs until its context is cancelled.
type Func func(context.Context) error

// Group runs components concurrently and ties their lifetimes together.
type Group struct {
	fns []Func
}

// Add registers a component.
func (g *Group) Add(fn Func) { g.fns = append(g.fns, fn) }

// Run starts every component. It returns once all of them have stopped, with
// the first error reported by any of them.
func (g *Group) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		once sync.Once
		err  error
	)
	for _, fn := range g.fns {
		wg.Add(1)
		go func(fn Func) {
			defer wg.Done()
			if e := fn(ctx); e != nil {
				once.Do(func() { err = e })
			}
			// Whichever component stops first takes the others with it.
			cancel()
		}(fn)
	}
	wg.Wait()
	return err
}
