package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"odysseia.local/backend/internal/blob"
)

func TestSweepAttachmentsRejectsBadArguments(t *testing.T) {
	_, err := sweepAttachments(context.Background(), nil, blob.NewMemory(), 0, attachmentSweepBatch)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("retention error = %v", err)
	}
	_, err = sweepAttachments(context.Background(), nil, blob.NewMemory(), time.Hour, 0)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("batch error = %v", err)
	}
	_, err = SweepAttachments(context.Background(), nil, nil, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "database pool") {
		t.Fatalf("dependency error = %v", err)
	}
}
