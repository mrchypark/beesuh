package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrchypark/beesuh"
)

func TestServiceOrganizationCannotOverridePersonalIdentity(t *testing.T) {
	f := newOAuthFixture(t)
	req := httptest.NewRequest("POST", "/v1/chat?organization_id=forged", strings.NewReader(`{"organization_id":"forged"}`))
	req.Header.Set("Authorization", "Bearer browser-token")
	req = req.WithContext(beesuh.WithPrincipal(req.Context(), beesuh.Principal{OrganizationID: "forged", UserID: "u", WorkspaceID: "w"}))
	p, err := f.s.authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.OrganizationID != "" || p.WorkspaceID != p.UserID {
		t.Fatalf("client input overrode personal identity: %+v", p)
	}
	p.OrganizationID = "forged"
	if err := f.s.authorize(req.Context(), p); !errors.Is(err, beesuh.ErrForbidden) {
		t.Fatalf("forged organization authorized: %v", err)
	}
	// An identity from trusted context is not a substitute for a human Bearer.
	if err := f.s.authorize(beesuh.WithPrincipal(context.Background(), p), p); err == nil {
		t.Fatal("principal without human token authorized")
	}
}
