package owners

import (
	"encoding/json"
	"strings"
)

const maxExtracted = 1000

// ExtractIDs returns the distinct non-empty strings found at field in a JSON
// body, in document order. Field syntax: dot-separated keys; a key ending in
// "[]" iterates an array ("data[].id"). Anything that does not match yields
// nothing; a non-JSON body yields nil.
func ExtractIDs(body []byte, field string) []string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var walk func(v any, parts []string)
	walk = func(v any, parts []string) {
		if len(out) >= maxExtracted {
			return
		}
		if len(parts) == 0 {
			if s, ok := v.(string); ok && s != "" && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
			return
		}
		key, iter := strings.CutSuffix(parts[0], "[]")
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		child, ok := obj[key]
		if !ok {
			return
		}
		if !iter {
			walk(child, parts[1:])
			return
		}
		arr, ok := child.([]any)
		if !ok {
			return
		}
		for _, e := range arr {
			walk(e, parts[1:])
		}
	}
	walk(v, strings.Split(field, "."))
	return out
}
