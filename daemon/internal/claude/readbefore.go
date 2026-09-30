package claude

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// ErrNotLineStart means an offset from an earlier read no longer falls between two lines: the file
// was replaced or rewritten since.
var ErrNotLineStart = errors.New("offset is not at the start of a line")

// ReadBefore returns the last n complete, non-blank lines that end at or before byte offset `end`
// (the end of the file when end < 0), reading at most maxBytes back from there. start is the offset
// of the first line returned, or of where reading stopped, so ReadBefore(path, start, …) continues
// further back with no overlap; 0 means the beginning was reached. next is the offset just past the
// last complete line, as ReadFrom returns it: an unfinished last line is left for ReadFrom. A line
// longer than maxBytes is skipped rather than returned in pieces.
func ReadBefore(path string, end int64, n int, maxBytes int64) (lines [][]byte, start, next, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, 0, err
	}
	size = st.Size()
	if end < 0 {
		end = size
	} else if end > size || (end > 0 && !newlineAt(f, end-1)) {
		return nil, 0, 0, size, ErrNotLineStart
	}
	from := max(end-maxBytes, 0)
	buf := make([]byte, end-from)
	if _, err := f.ReadAt(buf, from); err != nil && err != io.EOF {
		return nil, 0, 0, size, err
	}
	last := bytes.LastIndexByte(buf, '\n')
	if last < 0 {
		// Nothing complete in reach: one unfinished line, or one longer than maxBytes.
		if from == 0 {
			return nil, 0, 0, size, nil
		}
		s := lineStart(f, from)
		return nil, s, s, size, nil
	}
	next = from + int64(last) + 1
	// The window's first line is whole only when it starts the file or follows a newline.
	begin := 0
	if from > 0 && !newlineAt(f, from-1) {
		begin = bytes.IndexByte(buf, '\n') + 1
	}
	if begin > last {
		// The only line ending in reach started before it: skip that over-long line.
		return nil, lineStart(f, from), next, size, nil
	}
	type span struct {
		at   int64
		line []byte
	}
	var all []span
	at := begin
	for _, p := range bytes.Split(buf[begin:last], []byte{'\n'}) {
		if q := bytes.TrimRight(p, "\r"); len(bytes.TrimSpace(q)) > 0 {
			all = append(all, span{from + int64(at), q})
		}
		at += len(p) + 1
	}
	start = from + int64(begin)
	if len(all) > n {
		all = all[len(all)-n:]
		start = all[0].at
	}
	for _, s := range all {
		lines = append(lines, s.line)
	}
	return lines, start, next, size, nil
}

func newlineAt(f *os.File, off int64) bool {
	b := []byte{0}
	_, err := f.ReadAt(b, off)
	return err == nil && b[0] == '\n'
}

// lineStart finds the start of the line holding byte offset pos, scanning back in chunks.
func lineStart(f *os.File, pos int64) int64 {
	chunk := make([]byte, 64<<10)
	for pos > 0 {
		from := max(pos-int64(len(chunk)), 0)
		b := chunk[:pos-from]
		if _, err := f.ReadAt(b, from); err != nil && err != io.EOF {
			return 0
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return from + int64(i) + 1
		}
		pos = from
	}
	return 0
}
