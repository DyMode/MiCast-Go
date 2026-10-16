package playback

import (
	"context"
	"testing"
)

func TestPCMQueueReportsActualOverflowWithoutCountingCancellation(t *testing.T) {
	q := NewPCMQueue(context.Background())
	defer q.Close()
	for i := 0; i < cap(q.data); i++ {
		if !q.Push([]byte{1, 2, 3, 4}) {
			t.Fatal("queue unexpectedly full")
		}
	}
	if q.Push([]byte{1, 2, 3, 4}) || q.DroppedBytes() != 4 {
		t.Fatal("overflow must be visible", q.DroppedBytes())
	}
	q.Close()
	if q.Push([]byte{1, 2, 3, 4}) || q.DroppedBytes() != 4 {
		t.Fatal("normal cancellation counted as lost audio")
	}
}
