package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const (
	FileReaderLabel   = "dockpaum.file-reader"
	FileReaderPrefix  = "dockpaum-file-reader-"
	fileReaderTimeout = 30 * time.Second
	missingBindSource = "bind source path does not exist: "
)

const fileReaderScript = `m=$1 n=$2
shift 2
for f in "$@"; do
  printf '%s\n' "$m"
  if [ -L "$f" ]; then s=link
  elif [ ! -e "$f" ]; then s=missing
  elif [ ! -f "$f" ]; then s=other
  elif [ ! -r "$f" ]; then s=unreadable
  elif head -c "$n" "$f" 2>/dev/null; then s=file
  else s=error
  fi
  printf '\n%s %s\n' "$m" "$s"
done
`

type FileReader struct {
	Image   string
	Name    string
	MaxSize int64
	Labels  map[string]string
}

type HostFile struct {
	Data []byte
	Err  error
}

func IsFileReader(ct Container) bool {
	_, ok := ct.Labels[FileReaderLabel]
	return ok
}

func (c *Client) ReadHostFiles(ctx context.Context, r FileReader, dirs []string) (map[string]HostFile, error) {
	out := make(map[string]HostFile, len(dirs))
	var uniq []string
	for _, d := range dirs {
		if _, ok := out[d]; !ok && d != "" {
			out[d] = HostFile{}
			uniq = append(uniq, d)
		}
	}
	statuses, err := c.readMounted(ctx, r, uniq, false)
	if err != nil {
		return nil, err
	}
	var links []string
	for _, d := range uniq {
		st := statuses[d]
		if st.kind == "link" {
			links = append(links, HostPath(d, r.Name))
			continue
		}
		out[d] = st.result(r.MaxSize)
	}
	if len(links) == 0 {
		return out, nil
	}
	linked, err := c.readMounted(ctx, r, links, true)
	for _, d := range uniq {
		if statuses[d].kind != "link" {
			continue
		}
		if err != nil {
			out[d] = HostFile{Err: fmt.Errorf("follow the symlink: %w", err)}
			continue
		}
		st := linked[HostPath(d, r.Name)]
		if st.kind == "link" {
			st = readStatus{kind: "other"}
		}
		out[d] = st.result(r.MaxSize)
	}
	return out, nil
}

type readStatus struct {
	kind string
	data []byte
}

func (s readStatus) result(maxSize int64) HostFile {
	switch s.kind {
	case "file":
		if int64(len(s.data)) > maxSize {
			return HostFile{Err: fmt.Errorf("larger than the %d bytes allowed", maxSize)}
		}
		return HostFile{Data: s.data}
	case "missing":
		return HostFile{Err: fs.ErrNotExist}
	case "other":
		return HostFile{Err: errors.New("not a regular file")}
	case "unreadable":
		return HostFile{Err: fs.ErrPermission}
	}
	return HostFile{Err: errors.New("read error")}
}

func (c *Client) readMounted(ctx context.Context, r FileReader, sources []string, files bool) (map[string]readStatus, error) {
	out := make(map[string]readStatus, len(sources))
	remaining := append([]string(nil), sources...)
	for len(remaining) > 0 {
		missing, statuses, err := c.runFileReader(ctx, r, remaining, files)
		if err != nil {
			return nil, err
		}
		if missing == "" {
			maps.Copy(out, statuses)
			return out, nil
		}
		kept := remaining[:0:0]
		for _, s := range remaining {
			if s == missing {
				out[s] = readStatus{kind: "missing"}
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) == len(remaining) {
			return nil, fmt.Errorf("create the file reader container: %s%s", missingBindSource, missing)
		}
		remaining = kept
	}
	return out, nil
}

func (c *Client) runFileReader(ctx context.Context, r FileReader, sources []string, files bool) (string, map[string]readStatus, error) {
	nonce, err := newNonce()
	if err != nil {
		return "", nil, err
	}
	mounts := make([]mount.Mount, 0, len(sources))
	args := []string{nonce, strconv.FormatInt(r.MaxSize+1, 10)}
	for i, s := range sources {
		target := "/mnt/" + strconv.Itoa(i)
		mounts = append(mounts, mount.Mount{Type: mount.TypeBind, Source: s, Target: target, ReadOnly: true})
		if files {
			args = append(args, target)
		} else {
			args = append(args, target+"/"+r.Name)
		}
	}
	labels := maps.Clone(r.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels[FileReaderLabel]; !ok {
		labels[FileReaderLabel] = "true"
	}
	cctx, cancel := opCtx(ctx, opTimeout)
	res, err := c.api.ContainerCreate(cctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:           r.Image,
			Entrypoint:      []string{"/bin/sh", "-c", fileReaderScript, "dockpaum-file-reader"},
			Cmd:             args,
			User:            "0",
			Labels:          labels,
			NetworkDisabled: true,
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			Mounts:         mounts,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			CapAdd:         []string{"DAC_OVERRIDE"},
			SecurityOpt:    []string{"no-new-privileges", "label=disable"},
			LogConfig:      container.LogConfig{Type: "json-file"},
		},
		Name: fmt.Sprintf("%s%d", FileReaderPrefix, time.Now().UnixNano()),
	})
	cancel()
	if err != nil {
		if _, src, ok := strings.Cut(err.Error(), missingBindSource); ok {
			src = strings.TrimSpace(src)
			for _, s := range sources {
				if s == src {
					return s, nil, nil
				}
			}
		}
		return "", nil, fmt.Errorf("create the file reader container: %w", err)
	}
	defer func() {
		rctx, rcancel := opCtx(context.WithoutCancel(ctx), opTimeout)
		defer rcancel()
		_, _ = c.api.ContainerRemove(rctx, res.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}()
	if err := c.StartContainer(ctx, res.ID); err != nil {
		return "", nil, fmt.Errorf("start the file reader container: %w", err)
	}
	code, err := c.WaitExit(ctx, res.ID, fileReaderTimeout)
	if err != nil {
		return "", nil, fmt.Errorf("wait for the file reader container: %w", err)
	}
	limit := int64(len(sources))*(r.MaxSize+int64(2*len(nonce))+32) + 1
	stdout, stderr, err := c.readerLogs(ctx, res.ID, limit)
	if err != nil {
		return "", nil, fmt.Errorf("read the output of the file reader container: %w", err)
	}
	if code != 0 {
		return "", nil, fmt.Errorf("the file reader container exited with code %d: %s", code, firstLine(stderr))
	}
	statuses, err := parseReaderOutput(stdout, nonce, sources)
	if err != nil {
		return "", nil, err
	}
	return "", statuses, nil
}

func (c *Client) readerLogs(ctx context.Context, id string, limit int64) (stdout, stderr []byte, err error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	rc, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return nil, nil, err
	}
	defer rc.Close()
	var out bytes.Buffer
	errBuf := &limitedBuffer{max: 1024}
	if _, err := stdcopy.StdCopy(&out, errBuf, io.LimitReader(rc, limit+64*1024)); err != nil {
		return nil, nil, err
	}
	if int64(out.Len()) > limit {
		return nil, nil, errors.New("output larger than expected")
	}
	return out.Bytes(), errBuf.Bytes(), nil
}

func parseReaderOutput(stdout []byte, nonce string, sources []string) (map[string]readStatus, error) {
	out := make(map[string]readStatus, len(sources))
	rest := stdout
	head := []byte(nonce + "\n")
	tail := []byte("\n" + nonce + " ")
	for _, s := range sources {
		if !bytes.HasPrefix(rest, head) {
			return nil, errors.New("unexpected output from the file reader container")
		}
		rest = rest[len(head):]
		i := bytes.Index(rest, tail)
		if i < 0 {
			return nil, errors.New("truncated output from the file reader container")
		}
		data := rest[:i]
		rest = rest[i+len(tail):]
		kind, after, ok := bytes.Cut(rest, []byte("\n"))
		if !ok {
			return nil, errors.New("truncated output from the file reader container")
		}
		rest = after
		st := readStatus{kind: string(kind)}
		if st.kind == "file" {
			st.data = bytes.Clone(data)
		}
		out[s] = st
	}
	return out, nil
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if line == "" {
		return "no output"
	}
	return line
}

func HostPath(dir, name string) string {
	sep := "/"
	if strings.Contains(dir, `\`) && !strings.Contains(dir, "/") {
		sep = `\`
	}
	return strings.TrimRight(dir, `/\`) + sep + name
}
