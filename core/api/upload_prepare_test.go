package api

import (
	"errors"
	"io"
	"testing"
)

type faultingMultipartFile struct {
	readErr error
	seekErr error
}

func (f *faultingMultipartFile) Read([]byte) (int, error) { return 0, f.readErr }
func (f *faultingMultipartFile) ReadAt([]byte, int64) (int, error) {
	return 0, f.readErr
}
func (f *faultingMultipartFile) Seek(int64, int) (int64, error) {
	return 0, f.seekErr
}
func (f *faultingMultipartFile) Close() error { return nil }

func TestSniffMultipartMIMERejectsReadFailure(t *testing.T) {
	want := errors.New("read failed")
	_, err := sniffMultipartMIME(&faultingMultipartFile{readErr: want}, 512)
	if !errors.Is(err, want) {
		t.Fatalf("sniff error = %v, want %v", err, want)
	}
}

func TestSniffMultipartMIMERejectsRewindFailure(t *testing.T) {
	want := errors.New("seek failed")
	file := &faultingMultipartFile{readErr: io.EOF, seekErr: want}
	_, err := sniffMultipartMIME(file, 0)
	if !errors.Is(err, want) {
		t.Fatalf("sniff error = %v, want %v", err, want)
	}
}
