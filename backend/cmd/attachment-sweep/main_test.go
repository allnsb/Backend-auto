package main

import (
	"context"
	"strings"
	"testing"
)

func TestAttachmentSweepCommandRejectsBadInvocation(t *testing.T) {
	if err := run(context.Background(), []string{"once"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("usage error = %v", err)
	}
	t.Setenv("ODYSSEIA_ATTACHMENT_RETENTION", "super-secret-value")
	err := run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "ODYSSEIA_ATTACHMENT_RETENTION") || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("retention error leaked or accepted the value: %v", err)
	}
	t.Setenv("ODYSSEIA_ATTACHMENT_RETENTION", "168h")
	t.Setenv("ODYSSEIA_DATABASE_URL", "")
	err = run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "ODYSSEIA_DATABASE_URL") {
		t.Fatalf("missing database url error = %v", err)
	}
}
