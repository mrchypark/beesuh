package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCleanupUsesActualIDAndBothInventories(t *testing.T) {
	id := strings.Repeat("a", 64)
	prefix := "beesuh-live-unit"
	present := true
	removals := 0
	labelScans, nameScans := 0, 0
	run := func(args ...string) ([]byte, error) {
		switch args[0] {
		case "ps":
			filter := args[4]
			if strings.HasPrefix(filter, "label=") {
				labelScans++
			} else {
				nameScans++
			}
			if present {
				return []byte(id + " " + prefix + "-auth\n"), nil
			}
			return nil, nil
		case "inspect":
			if args[len(args)-1] != id {
				t.Fatal("inspected a name instead of actual ID")
			}
			if present {
				return []byte("false"), nil
			}
			return []byte("error: no such object"), errors.New("not found")
		case "rm":
			if args[1] != id {
				t.Fatal("removed a name instead of actual ID")
			}
			removals++
			if removals == 2 {
				present = false
			}
			return nil, nil
		case "network":
			if args[1] == "ls" {
				return nil, nil
			}
		}
		t.Fatalf("unexpected command %v", args)
		return nil, nil
	}
	if e := cleanupDocker(run, prefix, []string{id}, ""); e != nil {
		t.Fatal(e)
	}
	if present || removals != 2 || labelScans == 0 || nameScans == 0 {
		t.Fatal("cleanup did not retry deletion and verify both inventories")
	}
}
func TestCleanupFailsForHiddenIDOrUnavailableDaemon(t *testing.T) {
	id := strings.Repeat("b", 64)
	for _, unavailable := range []bool{false, true} {
		run := func(args ...string) ([]byte, error) {
			if unavailable {
				return []byte("daemon unavailable"), errors.New("daemon")
			}
			switch args[0] {
			case "ps":
				return nil, nil
			case "inspect":
				return []byte("false"), nil
			case "rm":
				return nil, nil
			case "network":
				return nil, nil
			}
			return nil, nil
		}
		if e := cleanupDocker(run, "beesuh-live-unit", []string{id}, ""); e == nil || !strings.Contains(e.Error(), id) {
			t.Fatal("cleanup falsely passed or omitted owned ID")
		}
	}
}
func TestProbeTLSRejectsUntrustedCertificateAndPlainHTTP(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	if e := probeTLS(context.Background(), server.URL); e == nil {
		t.Fatal("accepted an untrusted certificate")
	}
	if e := probeTLS(context.Background(), "http://127.0.0.1/"); e == nil {
		t.Fatal("accepted plaintext probe")
	}
}
