package beesuh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/rhiza"
)

// This exercises same-version process recovery from local durable state, not
// checkpoint restore or recovery after loss of the data directory.
func TestSingleNodeDurableRecovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected model endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var in struct{ Messages []model.Message }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode model request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch n := calls.Add(1); n {
		case 1:
			completion(w, "committed before kill", "stop")
		case 2:
			found := false
			for _, m := range in.Messages {
				found = found || m.Role == "assistant" && m.Content == "committed before kill"
			}
			if !found {
				t.Error("new run did not send recovered assistant history to the model")
			}
			completion(w, "completed after restart", "stop")
		default:
			t.Errorf("unexpected model dispatch %d (duplicate replay must not dispatch)", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	start := func(phase string) (*exec.Cmd, <-chan error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSingleNodeDurableRecoveryHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "BEESUH_RECOVERY_PHASE="+phase, "BEESUH_RECOVERY_DIR="+dir, "BEESUH_RECOVERY_MODEL="+server.URL)
		logPath := filepath.Join(dir, phase+".log")
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		t.Logf("%s subprocess pid=%d", phase, cmd.Process.Pid)
		done := make(chan error, 1)
		go func() {
			err := cmd.Wait()
			_ = log.Close()
			done <- err
			close(done)
		}()
		t.Cleanup(func() {
			cancel()
			<-done // Wait always reaps our child before TempDir cleanup.
			if t.Failed() {
				body, err := os.ReadFile(logPath)
				t.Logf("%s subprocess log (%v):\n%s", phase, err, body)
			}
		})
		return cmd, done
	}

	seed, seedDone := start("seed")
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(40 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, "committed.json")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-seedDone:
			t.Fatalf("seed exited before commit: %v", err)
		case <-deadline.C:
			t.Fatal("timed out waiting for durable commit")
		case <-ticker.C:
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("before crash model calls=%d, want 1", calls.Load())
	}
	if err := seed.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err := <-seedDone
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("seed termination=%v, want SIGKILL", err)
	}
	t.Logf("pid=%d killed with SIGKILL and reaped; reopening same DataDir", seed.Process.Pid)
	_, verifyDone := start("verify")
	if err := <-verifyDone; err != nil {
		t.Fatalf("recovery subprocess: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("total model calls=%d, want exactly 2", calls.Load())
	}
	t.Log("committed run and exact journal preserved; replay dispatched zero model calls; new run used recovered history")
}

func TestSingleNodeDurableRecoveryHelper(t *testing.T) {
	phase := os.Getenv("BEESUH_RECOVERY_PHASE")
	if phase == "" {
		t.Skip("subprocess helper")
	}
	if phase != "seed" && phase != "verify" {
		t.Fatalf("invalid helper phase %q", phase)
	}
	dir := os.Getenv("BEESUH_RECOVERY_DIR")
	opts := nativeOptions(filepath.Join(dir, "data"), dir, os.Getenv("BEESUH_RECOVERY_MODEL"), nil)
	opts.Timeout = 15 * time.Second
	r := nativeOpen(t, opts)
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	db, ok := r.db.(*rhiza.DB)
	if !ok {
		t.Fatalf("database is %T, want real embedded *rhiza.DB", r.db)
	}
	if !db.Ready() {
		t.Fatal("embedded Rhiza is not ready")
	}
	status, err := db.MembershipStatus()
	if err == nil && (status.NodeID != "beesuh" || len(status.Voters) != 1 || status.Voters[0] != "beesuh" || !status.Voting || status.Pending) {
		t.Fatalf("not a ready single voter: %+v, err=%v", status, err)
	}
	if err != nil {
		// Rhiza v0.20.0 gates MembershipStatus on AdminToken, which Open omits.
		// Its node.loadClusterConfig defaults empty Members to [beesuh], quorum 1.
		if !errors.Is(err, rhiza.ErrNotReady) {
			t.Fatalf("membership status: %v", err)
		}
		t.Log("ready embedded Rhiza; membership status unavailable without AdminToken; single-voter default verified in source")
	} else {
		t.Logf("actual Rhiza membership: node=%s voters=%v voting=%v config=%d; majority quorum=1", status.NodeID, status.Voters, status.Voting, status.ConfigID)
	}
	req := Request{WorkspaceID: "recovery", SessionKey: "conversation", RequestID: "committed", Prompt: Prompt{Text: "first request"}}
	type snapshot struct {
		Result  Result
		History []JournalEntry
	}
	path := filepath.Join(dir, "committed.json")
	ctx := t.Context()
	if phase == "seed" {
		out, err := r.Run(ctx, req)
		if err != nil || out.State != "succeeded" || string(out.Output) != `{"text":"committed before kill"}` {
			t.Fatalf("seed run=%+v err=%v", out, err)
		}
		history, err := r.History(ctx, req.WorkspaceID, req.SessionKey)
		if err != nil || len(history) == 0 {
			t.Fatalf("seed history=%+v err=%v", history, err)
		}
		body, err := json.Marshal(snapshot{out, history})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".tmp", body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
		// No Close: the parent kills this process after observing the commit.
		<-ctx.Done()
		t.Fatal("seed must be killed by parent")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want snapshot
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetRun(ctx, req.WorkspaceID, req.RequestID)
	if err != nil || !reflect.DeepEqual(got, want.Result) {
		t.Fatalf("recovered run=%+v want=%+v err=%v", got, want.Result, err)
	}
	history, err := r.History(ctx, req.WorkspaceID, req.SessionKey)
	if err != nil || !reflect.DeepEqual(history, want.History) {
		t.Fatalf("recovered journal=%+v want=%+v err=%v", history, want.History, err)
	}
	replay, err := r.Run(ctx, req)
	if err != nil || !reflect.DeepEqual(replay, want.Result) {
		t.Fatalf("replay=%+v want=%+v err=%v", replay, want.Result, err)
	}
	history, err = r.History(ctx, req.WorkspaceID, req.SessionKey)
	if err != nil || !reflect.DeepEqual(history, want.History) {
		t.Fatalf("replay changed journal: %+v err=%v", history, err)
	}
	req.RequestID, req.Prompt.Text = "after-restart", "second request"
	out, err := r.Run(ctx, req)
	if err != nil || out.State != "succeeded" || out.SessionID != want.Result.SessionID || string(out.Output) != `{"text":"completed after restart"}` {
		t.Fatalf("new run=%+v err=%v", out, err)
	}
}
