package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"odysseia.local/backend/internal/forum"
)

func TestWriteCapacityResponseContract(t *testing.T) {
	w := httptest.NewRecorder()
	writeAPIError(context.Background(), w, fmt.Errorf("waiting for write capacity: %w", forum.ErrBusy))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatal("temporary write exhaustion must provide 503 and Retry-After")
	}
	var problem Problem
	if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "write_capacity_exceeded" || problem.Status != w.Code {
		t.Fatalf("unexpected overload problem: %+v", problem)
	}
	doc, err := openapi3.NewLoader().LoadFromData(openAPI)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := doc.Components.Schemas["Problem"].Value.VisitJSON(value); err != nil {
		t.Fatalf("overload response violates the published contract: %v", err)
	}
}
