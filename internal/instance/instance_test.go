package instance

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	l1, err := Acquire(path, Info{PID: 42, Listen: "127.0.0.1:7070"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path, Info{PID: 43, Listen: "127.0.0.1:1"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}
	got, err := Read(path)
	if err != nil || got.PID != 42 || got.Listen != "127.0.0.1:7070" {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	if err := l1.Update(Info{PID: 42, Listen: "127.0.0.1:9"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(path); got.Listen != "127.0.0.1:9" {
		t.Errorf("after Update: %+v", got)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Acquire(path, Info{PID: 44, Listen: "127.0.0.1:2"})
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	_ = l2.Release()
}

func TestParseErrors(t *testing.T) {
	if _, err := parse("pid=1\n"); err == nil {
		t.Error("missing listen must fail")
	}
	if _, err := Read(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("missing file must fail")
	}
}
