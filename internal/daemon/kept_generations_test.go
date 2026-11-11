package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/dsfaccini/fuse/fuseops"
)

// keptWarningRecord is a logged kept-generations warning.
type keptWarningRecord struct {
	Level           string `json:"level"`
	Msg             string `json:"msg"`
	Repo            string `json:"repo"`
	Kept            int    `json:"kept_generations"`
	Threshold       int    `json:"warn_kept_generations"`
	EstimatedBytes  int64  `json:"estimated_bytes"`
	PIDs            string `json:"pids"`
	Frees           string `json:"frees"`
	RaiseThreshold  string `json:"raise_threshold"`
	EstimateFailure string `json:"estimate_error"`
}

// Each file held open across HEAD changes keeps the generation its inode
// reads, so files opened at different HEADs keep a generation each. When the
// generations a prune keeps for open files reach the repo's threshold, the
// daemon logs one warning and status shows it. While the count stays at or
// above the threshold, status follows it and nothing more is logged. Once
// closes drop the count below it, status clears the warning, and the next
// time the count reaches it the daemon logs again.
func TestKeptGenerationsWarnOnceEachTimeTheyReachTheThreshold(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, backend                   string
		firstPIDs, abovePIDs, againPIDs string
	}{
		{name: "kernel", firstPIDs: "4242 (2 open files)", abovePIDs: "4242 (2 open files), 5151 (1 open file)", againPIDs: "5151 (1 open file), 6161 (1 open file)"},
		{name: "FSKit", backend: fusefs.BackendFSKit, firstPIDs: "unavailable on FSKit", abovePIDs: "unavailable on FSKit", againPIDs: "unavailable on FSKit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newOpenFileFixture(t)
			f.mount.backend = tt.backend
			logs := &lockedBuffer{}
			f.svc.logger = slog.New(slog.NewJSONHandler(logs, nil))
			f.svc.mu.Lock()
			f.rt.cfg.WarnKeptGenerations = 2
			cfg := f.rt.cfg
			f.svc.mu.Unlock()
			if err := f.svc.registry.AddRepo(f.ctx, cfg); err != nil {
				t.Fatal(err)
			}
			lock, err := acquireDaemonLock(f.svc.root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lock.Close() })
			f.svc.recordRuntimeState(f.rt)

			gen := f.oldGen
			// step commits new content for every file, then serves it.
			step := func() {
				t.Helper()
				gen++
				for _, name := range []string{"f0", "f1", "f2", "f3"} {
					if err := os.WriteFile(filepath.Join(f.workDir, name), fmt.Appendf(nil, "%s at %d\n", name, gen), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				f.moveHEAD(f.commitToMain(fmt.Sprintf("generation %d", gen)), gen)
			}
			// open opens name, looked up at the generation served now, for pid.
			open := func(name string, pid uint32) fuseops.HandleID {
				t.Helper()
				lookup := &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: name}
				if err := f.fs.LookUpInode(f.ctx, lookup); err != nil {
					t.Fatalf("LookUpInode %s: %v", name, err)
				}
				op := &fuseops.OpenFileOp{Inode: lookup.Entry.Child, OpContext: fuseops.OpContext{Pid: pid}}
				if err := f.fs.OpenFile(f.ctx, op); err != nil {
					t.Fatalf("OpenFile %s: %v", name, err)
				}
				t.Cleanup(func() { _ = f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: op.Handle}) })
				return op.Handle
			}
			closeFile := func(handle fuseops.HandleID) {
				t.Helper()
				if err := f.fs.ReleaseFileHandle(f.ctx, &fuseops.ReleaseFileHandleOp{Handle: handle}); err != nil {
					t.Fatal(err)
				}
			}
			warnings := func() []keptWarningRecord {
				t.Helper()
				var out []keptWarningRecord
				lines := bufio.NewScanner(strings.NewReader(logs.String()))
				for lines.Scan() {
					var rec keptWarningRecord
					if err := json.Unmarshal(lines.Bytes(), &rec); err != nil {
						t.Fatalf("log line %q: %v", lines.Text(), err)
					}
					if rec.Msg == logKeptGenerations {
						out = append(out, rec)
					}
				}
				return out
			}
			// checkStatus checks the kept count and warning status reports.
			// moveHEAD returns after the prune recorded them.
			checkStatus := func(kept int, warning string) {
				t.Helper()
				st, err := f.svc.ctl().Status(f.ctx, cfg.Name)
				if err != nil {
					t.Fatal(err)
				}
				if st.KeptGenerations != kept || st.WarnKeptGenerations != 2 || st.KeptGenerationsWarning != warning {
					t.Fatalf("status kept_generations=%d warn_kept_generations=%d warning=%q, want %d, 2, %q", st.KeptGenerations, st.WarnKeptGenerations, st.KeptGenerationsWarning, kept, warning)
				}
			}
			// wantWarning is the warning about kept with pids, with the size
			// the snapshot store estimates for kept now.
			wantWarning := func(kept []int64, pids string) (keptWarningRecord, string) {
				t.Helper()
				bytes, err := f.rt.snapshot.EstimateGenerationBytes(f.ctx, kept)
				if err != nil {
					t.Fatal(err)
				}
				if bytes <= 0 {
					t.Fatalf("estimate of generations %v = %d bytes, want more than 0", kept, bytes)
				}
				raise := "artifact-fs set-warn-kept-generations --name repro --count <n>"
				return keptWarningRecord{Level: "WARN", Msg: logKeptGenerations, Repo: "repro", Kept: len(kept), Threshold: 2, EstimatedBytes: bytes, PIDs: pids, Frees: "at the first HEAD change after these files close", RaiseThreshold: raise},
					fmt.Sprintf("open files keep %d older snapshot generations, about %.1f MB, reaching the warning threshold 2; pids holding the files: %s; the generations free at the first HEAD change after the files close; raise the threshold with %s", len(kept), float64(bytes)/1e6, pids, raise)
			}

			step()
			first := open("f0", 4242)
			step()
			second := open("f1", 4242)
			step()
			open("f2", 5151)
			checkStatus(1, "")
			if got := warnings(); len(got) != 0 {
				t.Fatalf("warnings below the threshold = %+v, want none", got)
			}

			step()
			wantRec, wantText := wantWarning([]int64{f.oldGen + 1, f.oldGen + 2}, tt.firstPIDs)
			if got := warnings(); len(got) != 1 || got[0] != wantRec {
				t.Fatalf("warnings once the count reached the threshold = %+v, want one: %+v", got, wantRec)
			}
			checkStatus(2, wantText)

			step()
			if got := warnings(); len(got) != 1 {
				t.Fatalf("warnings while the count stays above the threshold = %+v, want only the first", got)
			}
			_, aboveText := wantWarning([]int64{f.oldGen + 1, f.oldGen + 2, f.oldGen + 3}, tt.abovePIDs)
			checkStatus(3, aboveText)

			closeFile(first)
			closeFile(second)
			step()
			checkStatus(1, "")

			open("f3", 6161)
			step()
			step()
			got := warnings()
			wantRec, wantText = wantWarning([]int64{f.oldGen + 3, f.oldGen + 6}, tt.againPIDs)
			if len(got) != 2 || got[1] != wantRec {
				t.Fatalf("warnings once the count reached the threshold again = %+v, want a second: %+v", got, wantRec)
			}
			checkStatus(2, wantText)
		})
	}
}
