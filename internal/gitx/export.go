package gitx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Export writes the tree of commit sha into the empty directory dest, using
// only blob contents. No attributes, filters, export-ignore, export-subst, or
// .gitattributes from the tree apply, because the export does not use
// `git archive` or checkout.
//
// Refused, and so a clean_checkout failure: symlinks and submodules (gitlinks),
// any path component named ".git", absolute or ".." paths, and any path whose
// parent components are not plain directories when written. Every component
// is checked with Lstat before a write, so a directory created earlier in the
// export cannot be used to write outside dest.
func (o Owned) Export(sha, dest string) error {
	list, err := o.run("ls-tree", "-r", "-z", "--full-tree", sha)
	if err != nil {
		return err
	}
	entries, err := parseLsTree(list)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := checkRelPath(e.path); err != nil {
			return err
		}
		switch e.mode {
		case "100644", "100755":
		case "120000":
			return fmt.Errorf("export: symlink %q not supported in clean checkout", e.path)
		case "160000":
			return fmt.Errorf("export: submodule %q not supported in clean checkout", e.path)
		default:
			return fmt.Errorf("export: unsupported mode %s for %q", e.mode, e.path)
		}
	}
	return o.writeBlobs(entries, dest)
}

type lsEntry struct {
	mode, sha, path string
}

// parseLsTree parses `ls-tree -r -z` output: "<mode> <type> <sha>\t<path>\0".
func parseLsTree(out string) ([]lsEntry, error) {
	var result []lsEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("export: bad ls-tree record %q", rec)
		}
		fields := strings.Fields(rec[:tab])
		if len(fields) != 3 {
			return nil, fmt.Errorf("export: bad ls-tree header %q", rec[:tab])
		}
		if fields[1] != "blob" && fields[1] != "commit" {
			return nil, fmt.Errorf("export: unexpected object type %q", fields[1])
		}
		result = append(result, lsEntry{mode: fields[0], sha: fields[2], path: rec[tab+1:]})
	}
	return result, nil
}

// checkRelPath rejects paths that could leave dest or collide with git's own
// metadata. Paths come from git as raw bytes, so no unquoting is needed.
func checkRelPath(p string) error {
	if p == "" || path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("export: unsafe path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".", "..":
			return fmt.Errorf("export: unsafe path component in %q", p)
		case ".git":
			return fmt.Errorf("export: path %q contains .git", p)
		}
	}
	return nil
}

// writeBlobs streams all blob contents through one `cat-file --batch`, which
// reads objects by id and does not apply filters.
func (o Owned) writeBlobs(entries []lsEntry, dest string) error {
	if len(entries) == 0 {
		return nil
	}
	cmd := o.Command("cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}

	// Feed ids in a goroutine so reads and writes cannot deadlock.
	go func() {
		w := bufio.NewWriter(stdin)
		for _, e := range entries {
			fmt.Fprintln(w, e.sha)
		}
		w.Flush()
		stdin.Close()
	}()

	r := bufio.NewReader(stdout)
	var werr error
	for _, e := range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			werr = fmt.Errorf("export: read cat-file header: %w", err)
			break
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != e.sha {
			werr = fmt.Errorf("export: unexpected cat-file header %q for %s", strings.TrimSpace(header), e.sha)
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			werr = err
			break
		}
		cr := &countingReader{r: io.LimitReader(r, size)}
		if werr == nil {
			werr = writeFile(dest, e, cr)
		}
		// Discard whatever the writer did not consume, so the stream stays
		// aligned for the next header even after a failed write.
		if _, err := io.CopyN(io.Discard, r, size-cr.n); err != nil && werr == nil {
			werr = err
		}
		if _, err := r.ReadByte(); err != nil && werr == nil {
			werr = err
		}
		if werr != nil {
			break
		}
	}
	// Drain so git can exit, then wait for it.
	_, _ = io.Copy(io.Discard, r)
	if err := cmd.Wait(); err != nil && werr == nil {
		werr = fmt.Errorf("export: cat-file: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return werr
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// writeFile creates dest/<e.path> with the blob content. Each parent
// component is Lstat-checked and created as a real directory; any existing
// component that is a symlink or a non-directory is an error.
func writeFile(dest string, e lsEntry, content io.Reader) error {
	parts := strings.Split(e.path, "/")
	cur := dest
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
		case err == nil:
			return fmt.Errorf("export: %q: parent %q is not a directory", e.path, part)
		case os.IsNotExist(err):
			if err := os.Mkdir(cur, 0o755); err != nil {
				return fmt.Errorf("export: mkdir %q: %w", cur, err)
			}
		default:
			return err
		}
	}
	perm := os.FileMode(0o644)
	if e.mode == "100755" {
		perm = 0o755
	}
	target := filepath.Join(dest, filepath.FromSlash(e.path))
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("export: create %q: %w", e.path, err)
	}
	if _, err := io.Copy(f, content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
