package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func (s *Store) BuildTreeIndex(ctx context.Context, repo model.RepoConfig, headOID string) ([]model.BaseNode, error) {
	// -z: NUL-delimited output with raw paths (no C-quoting of non-ASCII names).
	nodes := []model.BaseNode{rootNode(repo.ID)}
	var blobOIDs []string
	blobIndex := map[string][]int{} // oid -> indices into nodes
	var parseErr error
	if err := s.streamTreeRecords(ctx, repo.GitDir, headOID, func(line string) {
		n, typ, ok := parseTreeRecord(repo.ID, line)
		if !ok {
			if parseErr == nil {
				parseErr = fmt.Errorf("invalid ls-tree record %q", line)
			}
			return
		}
		idx := len(nodes)
		nodes = append(nodes, n)
		if typ == "blob" && n.ObjectOID != "" {
			blobIndex[n.ObjectOID] = append(blobIndex[n.ObjectOID], idx)
			if len(blobIndex[n.ObjectOID]) == 1 {
				blobOIDs = append(blobOIDs, n.ObjectOID)
			}
		}
	}); err != nil {
		return nil, err
	}
	if parseErr != nil {
		return nil, parseErr
	}

	// Batch-resolve sizes using cat-file --batch-check. This reads from local
	// pack metadata and doesn't trigger network fetches on blobless clones.
	if err := s.batchResolveSizes(ctx, repo, nodes, blobOIDs, blobIndex); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		// Non-fatal: sizes remain "unknown" and reads will still work via
		// hydration. Log so operators can diagnose unexpected attr hydration.
		s.logger.Warn("batch size resolution failed, some file sizes will resolve on demand", "repo", repo.Name, "error", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return addImplicitDirs(repo.ID, nodes), nil
}

func (s *Store) streamTreeRecords(ctx context.Context, gitDir string, headOID string, fn func(string)) error {
	env, err := s.lazyFetchEnv()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "ls-tree", "-r", "-t", "-z", headOID)
	configureCancelableCommand(cmd)
	// A treeless clone fetches missing trees here.
	configureCommandSession(cmd)
	cmd.Env = append(append(os.Environ(), "GIT_DIR="+gitDir), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// A lazy fetch's server shapes this stderr, and redaction's cost grows
	// faster than its length.
	errBuf := &boundedGitError{limit: maxGitErrorBytes}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() {
		_ = stdout.Close()
	})
	defer stopClose()
	readErr := readNullDelimited(stdout, fn)
	waitErr := cmd.Wait()
	if readErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return errors.Join(readErr, contextErr)
		}
		return readErr
	}
	if waitErr != nil {
		msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
		if msg == "" {
			msg = auth.RedactString(waitErr.Error())
		}
		if contextErr := ctx.Err(); contextErr != nil {
			if isCancellationSignal(msg) {
				return contextErr
			}
			return errors.Join(errors.New(msg), contextErr)
		}
		return errors.New(msg)
	}
	return nil
}

func readNullDelimited(r io.Reader, fn func(string)) error {
	reader := bufio.NewReader(r)
	for {
		record, err := reader.ReadString('\x00')
		if err == nil && record != "" {
			record = strings.TrimSuffix(record, "\x00")
			if record != "" {
				fn(record)
			}
		}
		if errors.Is(err, io.EOF) {
			if record != "" {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func parseTreeRecord(repoID model.RepoID, line string) (model.BaseNode, string, bool) {
	parts := strings.SplitN(line, "\t", 2)
	if len(parts) != 2 {
		return model.BaseNode{}, "", false
	}
	meta := strings.Fields(parts[0])
	if len(meta) < 3 {
		return model.BaseNode{}, "", false
	}
	modeStr := meta[0]
	typ := meta[1]
	oid := meta[2]
	mode64, err := strconv.ParseUint(modeStr, 8, 32)
	if err != nil {
		return model.BaseNode{}, typ, false
	}
	mode := uint32(mode64)
	if typ == "commit" {
		if mode != 0o160000 {
			return model.BaseNode{}, typ, false
		}
		// A gitlink needs a directory placeholder or Git reports the tracked
		// submodule as deleted. ArtifactFS does not initialize its contents.
		return model.BaseNode{
			RepoID:    repoID,
			Path:      parts[1],
			Type:      "dir",
			Mode:      mode,
			ObjectOID: oid,
			SizeState: "known",
			SizeBytes: 0,
		}, typ, true
	}
	return model.BaseNode{
		RepoID:    repoID,
		Path:      parts[1],
		Type:      normalizeGitType(typ, mode),
		Mode:      mode,
		ObjectOID: oid,
		SizeState: "unknown",
		SizeBytes: 0,
	}, typ, true
}

func (s *Store) batchResolveSizes(ctx context.Context, repo model.RepoConfig, nodes []model.BaseNode, oids []string, index map[string][]int) error {
	if len(oids) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch-check", "--buffer")
	configureCancelableCommand(cmd)
	// GIT_NO_LAZY_FETCH prevents batch-check from fetching blob metadata from
	// the promisor remote on blobless clones. Without it, every blob OID
	// triggers a network round-trip, turning a millisecond operation into
	// minutes. Blobs reported as "missing" keep SizeState="unknown" and get
	// their size resolved during hydration.
	cmd.Env = append(os.Environ(), "GIT_DIR="+repo.GitDir, "GIT_NO_LAZY_FETCH=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	errBuf := &bytes.Buffer{}
	cmd.Stderr = errBuf
	if err := cmd.Start(); err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() {
		_ = stdin.Close()
		_ = stdout.Close()
	})
	defer stopClose()
	writeErrCh := make(chan error, 1)
	go func() {
		var writeErr error
		for _, oid := range oids {
			if _, writeErr = fmt.Fprintln(stdin, oid); writeErr != nil {
				break
			}
		}
		if closeErr := stdin.Close(); writeErr == nil {
			writeErr = closeErr
		}
		writeErrCh <- writeErr
	}()
	// Output format: "<oid> <type> <size>" or "<oid> missing"
	scan := bufio.NewScanner(stdout)
	for scan.Scan() {
		applyBatchCheckLine(nodes, index, scan.Text())
	}
	scanErr := scan.Err()
	writeErr := <-writeErrCh
	waitErr := cmd.Wait()
	if contextErr := ctx.Err(); contextErr != nil {
		return errors.Join(contextErr, writeErr, scanErr)
	}
	if writeErr != nil {
		return writeErr
	}
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		msg := auth.RedactString(strings.TrimSpace(errBuf.String()))
		if msg == "" {
			msg = auth.RedactString(waitErr.Error())
		}
		return errors.New(msg)
	}
	return nil
}

func applyBatchCheckLine(nodes []model.BaseNode, index map[string][]int, line string) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return
	}
	oid := fields[0]
	sizeStr := fields[2]
	sz, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return
	}
	for _, idx := range index[oid] {
		nodes[idx].SizeBytes = sz
		nodes[idx].SizeState = "known"
	}
}

func rootNode(repoID model.RepoID) model.BaseNode {
	return model.BaseNode{
		RepoID:    repoID,
		Path:      ".",
		Type:      "dir",
		Mode:      0o755,
		ObjectOID: "",
		SizeState: "known",
	}
}

func normalizeGitType(t string, mode uint32) string {
	// Symlinks are reported as type "blob" with mode 120000
	if mode&0o170000 == 0o120000 {
		return "symlink"
	}
	switch t {
	case "blob":
		return "file"
	case "tree":
		return "dir"
	default:
		return "file"
	}
}

func addImplicitDirs(repoID model.RepoID, nodes []model.BaseNode) []model.BaseNode {
	seen := map[string]bool{".": true}
	for _, n := range nodes {
		seen[n.Path] = true
	}
	for _, n := range nodes {
		d := filepath.Dir(n.Path)
		for d != "." && d != "/" && !seen[d] {
			seen[d] = true
			nodes = append(nodes, model.BaseNode{
				RepoID:    repoID,
				Path:      d,
				Type:      "dir",
				Mode:      0o755,
				SizeState: "known",
			})
			d = filepath.Dir(d)
		}
	}
	return nodes
}
