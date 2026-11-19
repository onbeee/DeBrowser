//go:build !windows

package capture

type noopObserver struct{ ch chan RawHookEvent }

func newHookObserver() HookObserver {
	return &noopObserver{ch: make(chan RawHookEvent)}
}

func (o *noopObserver) Start(int) error             { return nil }
func (o *noopObserver) Stop()                       {}
func (o *noopObserver) Events() <-chan RawHookEvent { return o.ch }
