package main

import "testing"

func TestPublishedAddress(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"0.0.0.0:32771\n[::]:32771\n", "127.0.0.1:32771"},
		{"127.0.0.1:8443\n", "127.0.0.1:8443"},
		{"[::]:8443\n", "127.0.0.1:8443"},
	} {
		got, e := publishedAddress([]byte(tc.input))
		if e != nil || got != tc.want {
			t.Fatalf("publishedAddress=%q,%v want=%q", got, e, tc.want)
		}
	}
	for _, input := range []string{"", "not-an-address"} {
		if _, e := publishedAddress([]byte(input)); e == nil {
			t.Fatal("accepted invalid published address")
		}
	}
}
func TestHiddenApprovalFields(t *testing.T) {
	body := []byte(`<form><input type="hidden" name="csrf_token" value="bound&amp;encoded"><input type="hidden" name="review_digest" value="review"></form>`)
	if got, e := hidden(body, "csrf_token"); e != nil || got != "bound&encoded" {
		t.Fatal("invalid CSRF form parsing")
	}
	if _, e := hidden(body, "absent"); e == nil {
		t.Fatal("missing field accepted")
	}
}
