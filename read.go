package main

import (
    "errors"
    "io"
)

// ErrReadLimit is returned when reading a message that is larger than the
// read limit set for the connection.
var ErrExceededLimit = errors.New("websocket: read limit exceeded")

// Modified version of io.LimitedReader
//
// A LimitedReader reads from R but limits the amount of
// data returned to just N bytes. Each call to Read
// updates N to reflect the new amount remaining.
// Read returns ErrExceededLimit when N <= 0 and EOF when the underlying R returns EOF.

// LimitReader returns a Reader that reads from r
// but stops with EOF after n bytes.
// The underlying implementation is a *LimitedReader.
func LimitReader(r io.Reader, n int64) io.Reader { return &LimitedReader{r, n} }

type LimitedReader struct {
    R io.Reader // underlying reader
    N int64     // max bytes remaining
}

func (l *LimitedReader) Read(p []byte) (n int, err error) {
    if l.N <= 0 {
        return 0, ErrExceededLimit // This line is different from io.LimitReader
    }
    if int64(len(p)) > l.N {
        p = p[0:l.N]
    }
    n, err = l.R.Read(p)
    l.N -= int64(n)
    return
}
