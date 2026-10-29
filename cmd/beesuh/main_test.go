package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/opencode"
)

func TestCloseWithError(t *testing.T) {
	runErr := errors.New("run failed")
	closeErr := errors.New("close failed")
	for _, tt := range []struct {
		name     string
		runErr   error
		closeErr error
	}{
		{"success", nil, nil},
		{"close failure", nil, closeErr},
		{"run failure", runErr, nil},
		{"both failures", runErr, closeErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got := func() (err error) {
				defer closeWithError(&err, func() error {
					calls++
					return tt.closeErr
				})
				return tt.runErr
			}()
			if calls != 1 {
				t.Fatalf("close called %d times, want 1", calls)
			}
			if tt.runErr == nil && tt.closeErr == nil && got != nil {
				t.Fatalf("successful run and close returned %v", got)
			}
			for _, want := range []error{tt.runErr, tt.closeErr} {
				if want != nil && !errors.Is(got, want) {
					t.Errorf("returned error %v does not preserve %v", got, want)
				}
			}
		})
	}
}

func TestStorageInitProcess(t *testing.T) {
	if os.Getenv("BEESUH_TEST_STORAGE_INIT") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				break
			}
		}
		flag.CommandLine = flag.NewFlagSet("beesuh", flag.ExitOnError)
		main()
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, append([]string{"-test.run=^TestStorageInitProcess$", "--"}, args...)...)
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "BEESUH_") && !strings.HasPrefix(value, "OPENCODE_") && !strings.HasPrefix(value, "CONDUCTOR_") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "BEESUH_TEST_STORAGE_INIT=1")
		return cmd.CombinedOutput()
	}
	data := filepath.Join(t.TempDir(), "data")
	for i := 0; i < 2; i++ {
		if out, err := invoke("-storage-init", "-data-dir", data); err != nil {
			t.Fatalf("init %d: %v\n%s", i, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(data, "rhiza")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-listen", "127.0.0.1:0"}, {"-message", "hello"}, {"-recover"}, {"-compos"}, {"-interactive"},
		{"-session", "test"}, {"-providers", "missing.json"}, {"-service-config", "missing.json"},
		{"-model", "ignored"}, {"-backend", "opencode"}, {"-message="}, {"-recover=false"}, {"unexpected"}, {"-unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "must-not-exist")
			argv := append([]string{"-storage-init", "-data-dir", missing}, args...)
			out, err := invoke(argv...)
			if err == nil {
				t.Fatalf("accepted %v: %s", args, out)
			}
			if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bad args opened data: %v", err)
			}
		})
	}
	db, err := beesuh.Open(context.Background(), beesuh.Options{DataDir: data, BackendID: "test", MaxConcurrent: 1, OpenCode: opencode.Config{BaseURL: "http://127.0.0.1:1", Directory: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	out, initErr := invoke("-storage-init", "-data-dir", data)
	closeErr := db.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if initErr == nil || !strings.Contains(string(out), "in use") {
		t.Fatalf("locked init: %v %s", initErr, out)
	}
}
