package gateway

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCompletedReadInvalidatesQueuedTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &upstreamIdleBody{ReadCloser: io.NopCloser(strings.NewReader("hello")), timeout: time.Hour, cancel: cancel}
	defer body.Close()
	if n, err := body.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("Read = %d, %v", n, err)
	}
	// Simulate a callback already queued when Stop ran. It must not cancel
	// the request while the proxy is writing the completed read downstream.
	body.expire(1)
	if ctx.Err() != nil {
		t.Fatal("completed read's callback canceled downstream processing")
	}
}
