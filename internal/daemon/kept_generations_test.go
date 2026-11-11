package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
// the count is below the threshold, status clears the warning, and the next
// time the count reaches it the daemon logs again. A threshold that
// set-warn-kept-generations changes applies from the first prune after the
// daemon's next registry sync. The opens carry the IDs of this process and
// its parent, so on Linux each resolves to itself through /proc.
func TestKeptGenerationsWarnOnceEachTimeTheyReachTheThreshold(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, backend string }{
		{name: "fake backend"},
		{name: "FSKit", backend: fusefs.BackendFSKit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newOpenFileFixture(t)
			f.mount.backend = tt.backend
			logs := &lockedBuffer{}
			f.svc.logger = slog.New(slog.NewJSONHandler(logs, nil))
			threshold := 2
			f.svc.mu.Lock()
			f.rt.cfg.WarnKeptGenerations = threshold
			// A registry sync keeps a runtime whose config it finds enabled
			// and current.
			f.rt.cfg.Enabled, f.rt.cfg.ConfigVersion = true, "kept-generations-test"
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
			self, parent := uint32(os.Getpid()), uint32(os.Getppid())

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
			// setThreshold changes the threshold as the CLI does, then syncs
			// the registry as the daemon's sync loop does.
			setThreshold := func(n int) {
				t.Helper()
				if err := f.svc.ctl().SetWarnKeptGenerations(f.ctx, cfg.Name, n); err != nil {
					t.Fatal(err)
				}
				if err := f.svc.syncRepos(f.ctx); err != nil {
					t.Fatal(err)
				}
				threshold = n
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
			// checkStatus checks what status reports. moveHEAD returns after
			// the prune recorded it.
			checkStatus := func(kept int, warning string) {
				t.Helper()
				st, err := f.svc.ctl().Status(f.ctx, cfg.Name)
				if err != nil {
					t.Fatal(err)
				}
				if st.KeptGenerations != kept || st.WarnKeptGenerations != threshold || st.KeptGenerationsWarning != warning {
					t.Fatalf("status kept_generations=%d warn_kept_generations=%d warning=%q, want %d, %d, %q", st.KeptGenerations, st.WarnKeptGenerations, st.KeptGenerationsWarning, kept, threshold, warning)
				}
			}
			checkLogged := func(n int) []keptWarningRecord {
				t.Helper()
				got := warnings()
				if len(got) != n {
					t.Fatalf("warnings = %+v, want %d", got, n)
				}
				return got
			}
			// wantWarning is the warning about kept, whose files the
			// processes in holders opened, with the size the snapshot store
			// estimates for kept now.
			wantWarning := func(kept []int64, holders map[uint32]int) (keptWarningRecord, string) {
				t.Helper()
				bytes, err := f.rt.snapshot.EstimateGenerationBytes(f.ctx, kept)
				if err != nil {
					t.Fatal(err)
				}
				if bytes <= 0 {
					t.Fatalf("estimate of generations %v = %d bytes, want more than 0", kept, bytes)
				}
				pids := "unavailable on FSKit"
				if tt.backend != fusefs.BackendFSKit {
					var listed []string
					for _, pid := range slices.Sorted(maps.Keys(holders)) {
						noun := "open files"
						if holders[pid] == 1 {
							noun = "open file"
						}
						listed = append(listed, fmt.Sprintf("%d (%d %s)", pid, holders[pid], noun))
					}
					pids = strings.Join(listed, ", ")
				}
				raise := "artifact-fs set-warn-kept-generations --name repro --count <n>"
				return keptWarningRecord{Level: "WARN", Msg: logKeptGenerations, Repo: "repro", Kept: len(kept), Threshold: threshold, EstimatedBytes: bytes, PIDs: pids, Frees: "at the first HEAD change after these files close", RaiseThreshold: raise},
					fmt.Sprintf("open files keep %d older snapshot generations, about %.1f MB, reaching the warning threshold %d; pids holding the files: %s; the generations free at the first HEAD change after the files close; raise the threshold with %s", len(kept), float64(bytes)/1e6, threshold, pids, raise)
			}
			g := func(n int64) int64 { return f.oldGen + n }

			step()
			first := open("f0", self)
			step()
			second := open("f1", self)
			step()
			open("f2", parent)
			checkStatus(1, "")
			checkLogged(0)

			step()
			wantRec, wantText := wantWarning([]int64{g(1), g(2)}, map[uint32]int{self: 2})
			if got := checkLogged(1); got[0] != wantRec {
				t.Fatalf("warning once the count reached the threshold = %+v, want %+v", got[0], wantRec)
			}
			checkStatus(2, wantText)

			step()
			checkLogged(1)
			_, wantText = wantWarning([]int64{g(1), g(2), g(3)}, map[uint32]int{self: 2, parent: 1})
			checkStatus(3, wantText)

			setThreshold(4)
			step()
			checkLogged(1)
			checkStatus(3, "")

			setThreshold(3)
			step()
			wantRec, wantText = wantWarning([]int64{g(1), g(2), g(3)}, map[uint32]int{self: 2, parent: 1})
			if got := checkLogged(2); got[1] != wantRec {
				t.Fatalf("warning once the count reached a lowered threshold = %+v, want %+v", got[1], wantRec)
			}
			checkStatus(3, wantText)

			closeFile(first)
			closeFile(second)
			step()
			checkLogged(2)
			checkStatus(1, "")

			setThreshold(2)
			open("f3", self)
			step()
			step()
			wantRec, wantText = wantWarning([]int64{g(3), g(8)}, map[uint32]int{self: 1, parent: 1})
			if got := checkLogged(3); got[2] != wantRec {
				t.Fatalf("warning once the count reached the threshold again = %+v, want %+v", got[2], wantRec)
			}
			checkStatus(2, wantText)
		})
	}
}

// On Linux a FUSE op carries the caller's thread ID. The warning names the
// thread group /proc lists for it, and a thread ID when /proc does not answer.
// Threads of one process share its entry. Other platforms send the process
// ID, which the warning names as it is.
func TestOpenFilePIDsNameTheOpenersProcesses(t *testing.T) {
	t.Parallel()
	statuses := map[string]string{
		"/proc/4242/status": "Name:\teditor\nTgid:\t4242\nPid:\t4242\n",
		"/proc/4243/status": "Name:\teditor\nTgid:\t4242\nPid:\t4243\n",
		"/proc/5151/status": "Name:\tlsp\nPid:\t5151\n",
	}
	readFile := func(path string) ([]byte, error) {
		if status, ok := statuses[path]; ok {
			return []byte(status), nil
		}
		return nil, os.ErrNotExist
	}
	pids := map[uint32]int{0: 1, 4242: 1, 4243: 2, 5151: 1, 6161: 1}
	for goos, want := range map[string]string{
		"linux":  "unknown (1 open file), 4242 (3 open files), thread 5151 (1 open file), thread 6161 (1 open file)",
		"darwin": "unknown (1 open file), 4242 (1 open file), 4243 (2 open files), 5151 (1 open file), 6161 (1 open file)",
	} {
		got := formatOpenFilePIDs(pids, func(id uint32) (uint32, bool) { return openerProcess(goos, readFile, id) })
		if got != want {
			t.Errorf("%s: pids = %q, want %q", goos, got, want)
		}
	}
}
