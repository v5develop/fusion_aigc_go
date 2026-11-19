package aigc

import (
	"testing"
	"time"
)

func TestPastDeadline(t *testing.T) {
	if PastDeadline(time.Now().Add(-time.Minute), 5*time.Minute) {
		t.Fatal("未到截止不应判超时")
	}
	if !PastDeadline(time.Now().Add(-6*time.Minute), 5*time.Minute) {
		t.Fatal("已过截止应判超时")
	}
}
