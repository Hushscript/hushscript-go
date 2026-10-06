package hushscript

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// fileSource is the bytes of one upload: a random-access reader of exactly size bytes.
type fileSource struct {
	r      io.ReaderAt
	size   int64
	closer io.Closer
}

// fileUnreadable is the PreflightError for a Path that cannot be opened, stat'd or read as a file; the
// operating system's error stays reachable for errors.Is(err, fs.ErrNotExist) and the like.
func fileUnreadable(red redactor, message string, cause error) *Error {
	return newSDKError(KindPreflight, "file_unreadable", message, cause, red, hintInputs{})
}

// openPath opens the file at path. The caller closes the source.
func openPath(path string, red redactor) (*fileSource, error) {
	file, err := os.Open(path) //nolint:gosec // opening the file the caller named is the point
	if err != nil {
		return nil, fileUnreadable(red, "The file could not be opened.", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = errors.New("not a regular file")
		}
		return nil, fileUnreadable(red, "The file could not be read: it is not a regular file.", err)
	}
	return &fileSource{r: file, size: info.Size(), closer: file}, nil
}

func (s *fileSource) close() {
	if s != nil && s.closer != nil {
		_ = s.closer.Close()
	}
}

func (s *fileSource) sizeMismatch(red redactor, detail string) *Error {
	return newSDKError(KindUpload, "stream_size_mismatch",
		fmt.Sprintf("The source did not match the declared size of %d bytes: %s.", s.size, detail), nil, red, hintInputs{})
}

// readPart fills buf with the bytes at off. A source with fewer bytes than its size is a size
// mismatch; any other read failure is its own error.
func (s *fileSource) readPart(buf []byte, off int64, red redactor) error {
	got, err := s.r.ReadAt(buf, off)
	if got == len(buf) {
		if err == nil || errors.Is(err, io.EOF) {
			return nil
		}
		return newSDKError(KindUpload, "source_read_failed", "The file could not be read.", err, red, hintInputs{})
	}
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return s.sizeMismatch(red, "it ended early")
	}
	return newSDKError(KindUpload, "source_read_failed", "The file could not be read.", err, red, hintInputs{})
}

// checkNoMoreBytes fails when the source holds a byte past its declared size.
func (s *fileSource) checkNoMoreBytes(red redactor) error {
	var one [1]byte
	got, err := s.r.ReadAt(one[:], s.size)
	if got > 0 {
		return s.sizeMismatch(red, "it holds more bytes")
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return newSDKError(KindUpload, "source_read_failed", "The file could not be read.", err, red, hintInputs{})
	}
	return nil
}
