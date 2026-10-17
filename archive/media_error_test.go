package archive

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyMediaErrorFloodWait(t *testing.T) {
	k, d := classifyMediaError(errors.New("FLOOD_WAIT_120"))
	if k != "flood_wait" || d == "" {
		t.Fatalf("kind=%q detail=%q", k, d)
	}
}
func TestClassifyMediaErrorTimeout(t *testing.T) {
	k, _ := classifyMediaError(context.DeadlineExceeded)
	if k != "timeout" {
		t.Fatalf("kind=%q", k)
	}
	var _ net.Error = timeoutErr{}
	k, _ = classifyMediaError(timeoutErr{})
	if k != "timeout" {
		t.Fatalf("net timeout kind=%q", k)
	}
}
func TestClassifyMediaErrorNotFoundIsTerminal(t *testing.T) {
	k, _ := classifyMediaError(errors.New("FILE_REFERENCE_EXPIRED"))
	if k != "not_found" {
		t.Fatalf("kind=%q", k)
	}
}
func TestRetryBackoffByKind(t *testing.T) {
	a := retryDelay("io", 1)
	b := retryDelay("io", 2)
	if a < 2*time.Minute || b < 4*time.Minute || a >= time.Hour+30*time.Second || b >= time.Hour+30*time.Second {
		t.Fatalf("backoff a=%s b=%s", a, b)
	}
}
