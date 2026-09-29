// Package tracesum turns a chottag trace log into a route table, the list of
// tunnelled hosts, and links between ids returned by one route and used in
// the path of a later one.
package tracesum

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

type Route struct {
	Method, Host, Path, Class, Form string
	Auth                            []string
	Statuses                        []int
	Count, Swapped                  int
	Upgrade                         bool
	FirstMark                       string
}

type Tunnel struct {
	Host, Form string
	Count      int
	Errors     []string
}

// Note is set on links found by matching only the hashed part of two ids
// whose clear prefixes differ, e.g. "cse_->session_", or whose path id has
// no prefix at all, e.g. "cse_->" for a bare-hash path id matching a
// "cse_"-prefixed response id.
type Link struct{ From, Field, To, Note string }

// Unfinished counts, for one templated route ("METHOD host path"), the
// streams whose "head" record has no "req" record with the same id: a stream
// still open when the log was read, one that died with the daemon, or one
// whose req landed in the next rotated file, since summarize reads one file
// (spec §4.2, M1c6a). Expected, not corruption.
type Unfinished struct {
	Route string
	Count int
}

type Summary struct {
	Routes     []Route
	Tunnels    []Tunnel
	Links      []Link
	Marks      []string
	Unfinished []Unfinished // sorted by Route
}

func routeName(r tracelog.Record) string { return r.Method + " " + r.Host + " " + r.Path }

// TraceOnMark is the mark `chottag trace on` appends when it opens a trace
// window (M2c spec §4).
const TraceOnMark = "trace on"

// SinceLastTraceOn returns recs from the last TraceOnMark mark on, in log
// order, the mark included. With no such mark it returns recs whole, which
// covers a `trace run` log.
func SinceLastTraceOn(recs []tracelog.Record) []tracelog.Record {
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Kind == "mark" && recs[i].Mark == TraceOnMark {
			return recs[i:]
		}
	}
	return recs
}

func Summarize(recs []tracelog.Record) Summary {
	var s Summary
	routes := map[string]*Route{}
	auth := map[string]map[string]bool{}
	status := map[string]map[int]bool{}
	tunnels := map[string]*Tunnel{}
	idSource := map[string]struct{ from, field string }{}
	type idSrc struct{ from, field, prefix string }
	idByHash := map[string]idSrc{} // hashed part of prefixed ids -> first source
	linkSeen := map[Link]bool{}
	lastMark := ""
	// A streamed response writes a "head" record at header time and its
	// "req" record at the end, both with the same ID (M1c6a). Pairing is by
	// count per id, not presence, so two streams that drew the same random
	// id still leave the right number unfinished.
	heads := map[string][]string{} // id -> route names of its head records, in T order
	ended := map[string]int{}      // id -> number of req records carrying it

	// A record's T is the request/tunnel start time, but long-lived requests
	// (SSE streams, RC long-polls) are only written to the log once they
	// finish, so they can trail later marks in log order. Sort by T (stable,
	// so same-instant records keep their log order) before attributing
	// FirstMark, so a request is attributed to the mark that was current when
	// it started, not whichever mark happened to be logged first.
	recs = append([]tracelog.Record(nil), recs...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].T.Before(recs[j].T) })

	for _, r := range recs {
		switch r.Kind {
		case "mark":
			lastMark = r.Mark
			s.Marks = append(s.Marks, r.Mark)
		case "tunnel":
			k := r.Form + " " + r.Host
			t := tunnels[k]
			if t == nil {
				t = &Tunnel{Host: r.Host, Form: r.Form}
				tunnels[k] = t
			}
			t.Count++
			if r.Err != "" && !contains(t.Errors, r.Err) {
				t.Errors = append(t.Errors, r.Err)
			}
		case "head":
			// Never counted as a request: route, status and swap counts
			// come from "req" records only, so they are unchanged by M1c6a.
			heads[r.ID] = append(heads[r.ID], routeName(r))
		case "req":
			if r.ID != "" {
				ended[r.ID]++
			}
			name := routeName(r)
			k := name + " " + r.Class + " " + r.Form
			rt := routes[k]
			if rt == nil {
				rt = &Route{Method: r.Method, Host: r.Host, Path: r.Path, Class: r.Class, Form: r.Form, FirstMark: lastMark}
				routes[k] = rt
				auth[k] = map[string]bool{}
				status[k] = map[int]bool{}
			}
			rt.Count++
			if r.Swapped {
				rt.Swapped++
			}
			if r.Upgrade != "" {
				rt.Upgrade = true
			}
			auth[k][r.Auth] = true
			status[k][r.Status] = true
			for _, h := range r.PathIDs {
				var l Link
				if src, ok := idSource[h]; ok {
					l = Link{From: src.from, Field: src.field, To: name}
				} else {
					// A path id with no clear prefix is just a bare hash, and
					// HashID("X") == hash8("X") == the hash part of a
					// prefixed id, so a bare path id can still match a
					// prefixed response id by hash.
					p, hh := tracelog.SplitIDLabel(h)
					src, ok := idByHash[hh]
					if !ok || (p != "" && src.prefix == p) {
						continue
					}
					note := src.prefix + "->" + p
					if p == "" {
						note = src.prefix + "->"
					}
					l = Link{From: src.from, Field: src.field, To: name, Note: note}
				}
				if !linkSeen[l] {
					linkSeen[l] = true
					s.Links = append(s.Links, l)
				}
			}
			walkIDs(r.RespShape, "", func(field, hash string) {
				if _, ok := idSource[hash]; !ok {
					idSource[hash] = struct{ from, field string }{name, field}
				}
				if p, hh := tracelog.SplitIDLabel(hash); p != "" {
					if _, ok := idByHash[hh]; !ok {
						idByHash[hh] = idSrc{name, field, p}
					}
				}
			})
		}
	}
	for k, rt := range routes {
		for a := range auth[k] {
			rt.Auth = append(rt.Auth, a)
		}
		sort.Strings(rt.Auth)
		for st := range status[k] {
			rt.Statuses = append(rt.Statuses, st)
		}
		sort.Ints(rt.Statuses)
		s.Routes = append(s.Routes, *rt)
	}
	sort.Slice(s.Routes, func(i, j int) bool {
		a, b := s.Routes[i], s.Routes[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Form < b.Form
	})
	unfinished := map[string]int{}
	for id, names := range heads {
		if n := ended[id]; n < len(names) {
			for _, name := range names[n:] {
				unfinished[name]++
			}
		}
	}
	for name, n := range unfinished {
		s.Unfinished = append(s.Unfinished, Unfinished{Route: name, Count: n})
	}
	sort.Slice(s.Unfinished, func(i, j int) bool { return s.Unfinished[i].Route < s.Unfinished[j].Route })
	for _, t := range tunnels {
		s.Tunnels = append(s.Tunnels, *t)
	}
	sort.Slice(s.Tunnels, func(i, j int) bool {
		a, b := s.Tunnels[i], s.Tunnels[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Form < b.Form
	})
	return s
}

// walkIDs calls fn for every "id:<hash>" leaf in a shape, with its field path.
func walkIDs(shape any, prefix string, fn func(field, hash string)) {
	switch x := shape.(type) {
	case map[string]any:
		for k, v := range x {
			walkIDs(v, prefix+"."+k, fn)
		}
	case []any:
		for _, v := range x {
			walkIDs(v, prefix+"[]", fn)
		}
	case string:
		if h, ok := strings.CutPrefix(x, "id:"); ok {
			fn(prefix, h)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func (s Summary) WriteText(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "# chottag trace summary")
	fmt.Fprintln(tw, "## Routes")
	fmt.Fprintln(tw, "METHOD\tHOST\tPATH\tCLASS\tFORM\tAUTH\tSTATUS\tCOUNT\tSWAPPED\tUPGRADE\tFIRST-SEEN-AFTER")
	for _, r := range s.Routes {
		st := make([]string, len(r.Statuses))
		for i, v := range r.Statuses {
			st[i] = fmt.Sprint(v)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%v\t%s\n", r.Method, r.Host, r.Path, r.Class, r.Form,
			strings.Join(r.Auth, ","), strings.Join(st, ","), r.Count, r.Swapped, r.Upgrade, r.FirstMark)
	}
	fmt.Fprintln(tw, "## Tunnels")
	fmt.Fprintln(tw, "HOST\tFORM\tCOUNT\tERRORS")
	for _, t := range s.Tunnels {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", t.Host, t.Form, t.Count, strings.Join(t.Errors, " | "))
	}
	fmt.Fprintln(tw, "## ID links (response field -> later request path)")
	for _, l := range s.Links {
		to := l.To
		if l.Note != "" {
			to += "  (id prefix " + l.Note + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t->\t%s\n", l.From, l.Field, to)
	}
	total := 0
	for _, u := range s.Unfinished {
		total += u.Count
	}
	fmt.Fprintln(tw, "## Unfinished streams (head record with no req: still open, or died with the daemon)")
	fmt.Fprintf(tw, "unfinished streams: %d\n", total)
	for _, u := range s.Unfinished {
		fmt.Fprintf(tw, "%s\t%d\n", u.Route, u.Count)
	}
	fmt.Fprintln(tw, "## Marks")
	for _, m := range s.Marks {
		fmt.Fprintln(tw, m)
	}
	return tw.Flush()
}
