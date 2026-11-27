// Package usageimport reads Claude Code transcripts (*.jsonl) and appends the
// assistant usage numbers to the usage ledger. Only the message id, request
// id, timestamp, model and usage numbers are decoded: never content, cwd or
// sessionId. Transcripts are opened read-only.
package usageimport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/HaiNNT/c-hottag/internal/usagelog"
)

// ctxCheckLines is how many transcript lines pass between ctx checks.
const ctxCheckLines = 1000

// Source is one transcript tree: Dir is a "projects" directory, Src the
// ledger source tag ("slot" or "home").
type Source struct{ Dir, Src string }

// Result counts what one Import pass saw. Skipped counts transcript files and
// folders that could not be read; they are left for the next pass.
type Result struct{ Files, Lines, Events, Skipped int }

// markKey is the import.json key of a transcript: the lowercase hex sha256 of
// its path, so the file never holds a path, project folder or session id.
func markKey(path string) string {
	h := sha256.Sum256([]byte(path))
	return hex.EncodeToString(h[:])
}

func isHexKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	for _, c := range k {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// openFile opens a transcript read-only. It is a var so tests can check the
// handle it hands back cannot write.
var openFile = func(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDONLY, 0) }

type usageJSON struct {
	In            *int64 `json:"input_tokens"`
	Out           *int64 `json:"output_tokens"`
	CR            *int64 `json:"cache_read_input_tokens"`
	CW            *int64 `json:"cache_creation_input_tokens"`
	Speed         string `json:"speed"`
	CacheCreation *struct {
		M5 int64 `json:"ephemeral_5m_input_tokens"`
		H1 int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Server *struct {
		Web int64 `json:"web_search_requests"`
	} `json:"server_tool_use"`
}

type line struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	RequestID string `json:"requestId"`
	Message   *struct {
		ID    string     `json:"id"`
		Model string     `json:"model"`
		Usage *usageJSON `json:"usage"`
	} `json:"message"`
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// parse returns the event for one transcript line, or false when the line
// does not count.
func parse(b []byte, src string, cutoff time.Time) (usagelog.Event, bool) {
	var ln line
	if json.Unmarshal(b, &ln) != nil || ln.Type != "assistant" || ln.Message == nil ||
		ln.Message.Usage == nil || ln.Message.Model == "" || ln.Message.Model == "<synthetic>" {
		return usagelog.Event{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ln.Timestamp)
	if err != nil || t.Before(cutoff) {
		return usagelog.Event{}, false
	}
	u := ln.Message.Usage
	ev := usagelog.Event{
		T: t, MID: ln.Message.ID, RID: ln.RequestID, Src: src,
		Model: ln.Message.Model, Speed: u.Speed,
		In: val(u.In), Out: val(u.Out), CR: val(u.CR),
	}
	if u.CacheCreation != nil {
		ev.CW5m, ev.CW1h = u.CacheCreation.M5, u.CacheCreation.H1
	} else {
		ev.CW5m = val(u.CW)
	}
	if u.Server != nil {
		ev.Web = u.Server.Web
	}
	return ev, true
}

// pass accumulates one import pass: the last line of a message id wins.
type pass struct {
	evs   []usagelog.Event
	index map[string]int
	lines int
}

func (p *pass) add(ev usagelog.Event) {
	p.lines++
	if ev.MID != "" {
		if i, ok := p.index[ev.MID]; ok {
			p.evs[i] = ev
			return
		}
		p.index[ev.MID] = len(p.evs)
	}
	p.evs = append(p.evs, ev)
}

// readFile reads path from start and returns the offset after the last
// complete line, plus the file info it saw.
func readFile(ctx context.Context, path string, start int64, src string, cutoff time.Time) (int64, fs.FileInfo, []usagelog.Event, error) {
	f, err := openFile(path)
	if err != nil {
		return 0, nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, nil, nil, err
	}
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return 0, nil, nil, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	offset := start
	var evs []usagelog.Event
	for n := 0; ; n++ {
		if n%ctxCheckLines == 0 {
			if err := ctx.Err(); err != nil {
				return 0, nil, nil, err
			}
		}
		b, rerr := r.ReadBytes('\n')
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return offset, fi, evs, nil // a partial last line waits for its newline
			}
			return 0, nil, nil, rerr
		}
		offset += int64(len(b))
		if ev, ok := parse(b, src, cutoff); ok {
			evs = append(evs, ev)
		}
	}
}

// Import appends new assistant usage from every *.jsonl under each source's
// Dir (recursive), skipping lines timestamped before cutoff. The caller holds
// l.Lock(). A cancelled ctx stops it between files and every ctxCheckLines
// lines, returning ctx.Err() with nothing appended and no marks saved, so the
// next run redoes the whole pass.
func Import(ctx context.Context, l usagelog.Ledger, srcs []Source, cutoff time.Time, reimport bool) (Result, error) {
	var res Result
	loaded, err := l.ImportState()
	if err != nil {
		return res, err
	}
	marks := make(map[string]usagelog.FileMark, len(loaded))
	for k, m := range loaded { // migrate: a key that is not a hash is a path
		if !isHexKey(k) {
			k = markKey(k)
		}
		marks[k] = m
	}
	p := &pass{index: map[string]int{}}
	seen := map[string]bool{}
	clean := true // every source walked without a gap, so unseen marks are gone files
	for _, s := range srcs {
		var paths []string
		rootGone := false
		err := filepath.WalkDir(s.Dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					if d == nil {
						rootGone = true
					}
					return nil
				}
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
				res.Skipped++
				clean = false
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() && filepath.Ext(path) == ".jsonl" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return res, fmt.Errorf("usageimport: walk: %w", err)
		}
		if rootGone {
			clean = false
		}
		sort.Strings(paths)
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			key := markKey(path)
			seen[key] = true
			fi, err := os.Stat(path)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					res.Skipped++
				}
				continue
			}
			var start int64
			if m, ok := marks[key]; ok && !reimport {
				if m.Size == fi.Size() && m.ModTime.Equal(fi.ModTime()) {
					continue
				}
				if fi.Size() > m.Size && !fi.ModTime().Before(m.ModTime) {
					start = m.Offset
				}
			}
			off, got, evs, err := readFile(ctx, path, start, s.Src, cutoff)
			if err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return res, cerr
				}
				if !errors.Is(err, fs.ErrNotExist) {
					res.Skipped++
				}
				continue
			}
			for _, ev := range evs {
				p.add(ev)
			}
			res.Files++
			marks[key] = usagelog.FileMark{Size: got.Size(), ModTime: got.ModTime(), Offset: off}
		}
	}
	if clean {
		for k := range marks {
			if !seen[k] {
				delete(marks, k)
			}
		}
	}
	res.Lines, res.Events = p.lines, len(p.evs)
	if len(p.evs) > 0 {
		if err := l.AppendEvents(p.evs); err != nil {
			return res, err
		}
	}
	return res, l.SaveImportState(marks)
}
