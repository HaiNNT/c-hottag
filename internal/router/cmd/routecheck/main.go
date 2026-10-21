// Command routecheck is maintainer tooling for the upstream-check skill's
// scan-bundle (R186). It reads request paths on stdin, one per line, and
// prints "explicit" or "default" for each: whether router.Explicit says a
// rule names that path on api.anthropic.com. It is not part of the chottag
// binary and prints nothing but those two words.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/HaiNNT/c-hottag/internal/router"
)

// methods is every method the router has a rule for; a path is explicit when
// a rule names it for any of them, in either request form.
var methods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

func check(path string) string {
	for _, m := range methods {
		for _, abs := range []bool{false, true} {
			if router.Explicit(router.Request{Method: m, Host: router.APIHost, Path: path, AbsoluteForm: abs}) {
				return "explicit"
			}
		}
	}
	return "default"
}

func run(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		p := strings.TrimSpace(sc.Text())
		if p == "" {
			continue
		}
		if _, err := fmt.Fprintln(out, check(p)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "routecheck:", err)
		os.Exit(1)
	}
}
