//go:build !windows

package capture

func StartCapture(h Handler) error { return ErrUnsupported }

func StopCapture() {}

func CurrentModifiers() int { return 0 }
