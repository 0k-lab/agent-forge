package processtree

import "errors"

// ErrStart means the requested target could not be started.
var ErrStart = errors.New("process start failed")
