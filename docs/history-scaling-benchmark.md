# History scaling benchmark

This benchmark measures the two real history read paths on a disposable embedded
Rhiza database. It does not execute a provider.

- `Runtime.History` reads the latest 200 journal rows. The target session always
  has 200 journal rows. The fixture adds 1,000, 10,000, or 100,000 rows for a
  different session in the same workspace.
- `nativeHistory` reads the latest eight successful turns. The target session
  always has eight successful runs and their journal messages. The fixture adds
  successful runs for a different session in the same workspace.

The command used ten timed calls for each fixture and repeated the process three
times. Fixed iteration avoids Go benchmark calibration recreating the large
disposable database. Database setup is outside the timed region.

```sh
GOCACHE=/tmp/beesuh-go-cache go test -v -run '^TestHistoryScalingFixture$' -bench '^BenchmarkHistoryScaling$' -benchtime=10x -count=3 .
```

Test output:

```
=== RUN   TestHistoryScalingFixture
=== RUN   TestHistoryScalingFixture/other=1000
=== RUN   TestHistoryScalingFixture/other=10000
=== RUN   TestHistoryScalingFixture/other=100000
--- PASS: TestHistoryScalingFixture (27.14s)
    --- PASS: TestHistoryScalingFixture/other=1000 (0.35s)
    --- PASS: TestHistoryScalingFixture/other=10000 (2.91s)
    --- PASS: TestHistoryScalingFixture/other=100000 (23.88s)
=== RUN   TestHistoryScalingFixture
=== RUN   TestHistoryScalingFixture/other=1000
=== RUN   TestHistoryScalingFixture/other=10000
=== RUN   TestHistoryScalingFixture/other=100000
--- PASS: TestHistoryScalingFixture (26.50s)
    --- PASS: TestHistoryScalingFixture/other=1000 (0.38s)
    --- PASS: TestHistoryScalingFixture/other=10000 (2.42s)
    --- PASS: TestHistoryScalingFixture/other=100000 (23.70s)
=== RUN   TestHistoryScalingFixture
=== RUN   TestHistoryScalingFixture/other=1000
=== RUN   TestHistoryScalingFixture/other=10000
=== RUN   TestHistoryScalingFixture/other=100000
--- PASS: TestHistoryScalingFixture (22.70s)
    --- PASS: TestHistoryScalingFixture/other=1000 (0.36s)
    --- PASS: TestHistoryScalingFixture/other=10000 (2.33s)
    --- PASS: TestHistoryScalingFixture/other=100000 (20.00s)
```

Benchmark output:

```
goos: darwin
goarch: arm64
pkg: github.com/mrchypark/beesuh
cpu: Apple M3
BenchmarkHistoryScaling
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=1000
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=1000-8         	      10	    318788 ns/op	  123896 B/op	    3547 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=1000-8         	      10	    310258 ns/op	  123892 B/op	    3547 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=1000-8         	      10	    742688 ns/op	  123892 B/op	    3547 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=1000
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=1000-8	      10	    757158 ns/op	   26498 B/op	     748 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=1000-8	      10	    498312 ns/op	   26496 B/op	     748 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=1000-8	      10	    471150 ns/op	   26498 B/op	     748 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=10000
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=10000-8        	      10	    603496 ns/op	  123890 B/op	    3547 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=10000-8        	      10	    362004 ns/op	  123892 B/op	    3547 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=10000-8        	      10	    332583 ns/op	  123892 B/op	    3547 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=10000
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=10000-8	      10	   3022967 ns/op	   26496 B/op	     748 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=10000-8	      10	   2844962 ns/op	   26485 B/op	     748 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=10000-8	      10	  17216350 ns/op	   26549 B/op	     749 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=100000
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=100000-8                	      10	    942112 ns/op	  123888 B/op	    3548 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=100000-8                	      10	    384883 ns/op	  123897 B/op	    3547 allocs/op
BenchmarkHistoryScaling/HistoryLatest200/OtherWorkspaceRecords=100000-8                	      10	    322200 ns/op	  123906 B/op	    3548 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=100000
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=100000-8        	      10	  23514704 ns/op	   26506 B/op	     749 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=100000-8        	      10	  24271421 ns/op	   26504 B/op	     749 allocs/op
BenchmarkHistoryScaling/NativeRecentSuccessfulTurns/OtherWorkspaceRuns=100000-8        	      10	  24234950 ns/op	   26510 B/op	     749 allocs/op
PASS
ok  	github.com/mrchypark/beesuh	259.343s
```

## Result

The latest-200 public history query is flat. Its three samples were 0.31–0.74
ms at 1,000 rows, 0.33–0.60 ms at 10,000 rows, and 0.32–0.94 ms at 100,000 rows.
Allocations stayed at 3,547 or 3,548 per operation and about 123,900 bytes.

The native recent-successful-turn query grows with unrelated retained runs. Its
range increased from 0.47–0.76 ms at 1,000 rows to 2.84–17.22 ms at 10,000 rows
and 23.51–24.27 ms at 100,000 rows. Its allocations stayed flat at 748 or 749
per operation and about 26,500 bytes. The one 17.22 ms 10,000-row sample shows
host variance, but every 100,000-row sample is about 31 to 51 times the 1,000-row
samples.

No Rhiza query counter exposes records examined. The stable allocation counts
and growing latency isolate the problem to database work rather than result
materialization.

The public query uses `journal_session(workspace,session_key,seq)`, so its
filtered order has an index. The native query filters `runs` by workspace,
session, and state, then orders by `rowid`, but `runs` only has the primary key
`(workspace,request_id)`. Add a contiguous migration with:

```sql
CREATE INDEX runs_workspace_session_state ON runs(workspace,session_key,state)
```

This is the smallest access-path change. The equality keys let Rhiza select the
target session and state before it reads the newest row IDs. This benchmark did
not change the access path because migration registration is owned by another
concurrent change.
