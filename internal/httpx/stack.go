package httpx

import "runtime"

// stack returns the current goroutine's stack, bounded so that a deep panic
// cannot produce an unbounded log record.
func stack() string {
	buf := make([]byte, 8<<10)
	n := runtime.Stack(buf, false)
	return string(buf[:n])
}
