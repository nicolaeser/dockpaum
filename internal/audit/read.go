package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

type Query struct {
	Containers []string
	Since      time.Time
	Limit      int
}

type ReadResult struct {
	Records   []Record
	Malformed int
}

const maxRecordBytes = 1 << 20

type monthFile struct {
	path       string
	start, end time.Time
}

func fileFor(dir, month string) string {
	return filepath.Join(dir, fmt.Sprintf("dockpaum-%s.jsonl", month))
}

func monthFiles(dir string) ([]monthFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []monthFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "dockpaum-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		start, err := time.Parse("2006-01", strings.TrimSuffix(strings.TrimPrefix(name, "dockpaum-"), ".jsonl"))
		if err != nil {
			continue
		}
		out = append(out, monthFile{path: filepath.Join(dir, name), start: start, end: start.AddDate(0, 1, 0)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start.Before(out[j].start) })
	return out, nil
}

func Read(dir string, q Query) (ReadResult, error) {
	if q.Limit < 0 {
		return ReadResult{}, errors.New("audit: limit must not be negative")
	}
	files, err := monthFiles(dir)
	if err != nil {
		return ReadResult{}, fmt.Errorf("audit: %w", err)
	}
	var res ReadResult
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		if !q.Since.IsZero() && !f.end.After(q.Since) {
			break
		}
		keep := 0
		if q.Limit > 0 {
			keep = q.Limit - len(res.Records)
		}
		recs, malformed, err := readFile(f.path, q, keep)
		if err != nil {
			return ReadResult{}, err
		}
		res.Malformed += malformed
		res.Records = append(recs, res.Records...)
		if q.Limit > 0 && len(res.Records) >= q.Limit {
			break
		}
	}
	return res, nil
}

func readFile(path string, q Query, keep int) ([]Record, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []Record
	malformed := 0
	trim := func() {
		sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
		if keep > 0 && len(out) > keep {
			out = append(out[:0], out[len(out)-keep:]...)
		}
	}
	overlong, err := scanLines(f, func(line []byte) {
		if len(line) == 0 {
			return
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil || rec.At.IsZero() {
			malformed++
			return
		}
		if !q.Since.IsZero() && rec.At.Before(q.Since) {
			return
		}
		if len(q.Containers) > 0 && !slices.Contains(q.Containers, rec.Container) {
			return
		}
		out = append(out, rec)
		if keep > 0 && len(out) >= 2*keep {
			trim()
		}
	})
	if err != nil {
		return nil, 0, fmt.Errorf("audit: read %s: %w", path, err)
	}
	trim()
	return out, malformed + overlong, nil
}

func scanLines(r io.Reader, fn func(line []byte)) (overlong int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	discarding := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !discarding {
			if len(buf)+len(chunk) > maxRecordBytes+1 {
				discarding = true
				buf = buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil, errors.Is(err, io.EOF):
			if discarding {
				overlong++
			} else if line := bytes.TrimSpace(buf); len(line) > 0 {
				fn(line)
			}
			buf, discarding = buf[:0], false
			if err != nil {
				return overlong, nil
			}
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			return overlong, err
		}
	}
}
