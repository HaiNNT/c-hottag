package tracelog

import (
	"bytes"
	"encoding/json"
)

const maxShapeDepth = 8

// Shape returns the structure of a JSON document with each value replaced by
// its type; id-like strings become "id:<hash>", secret-like ones "secret".
// Object keys are types-only too: a secret-like key becomes "{secret}" and
// an identifier-like key (see isIDKey) becomes "{key}", so UUIDs, emails,
// repo/path-like names and ULID/base62 ids never persist verbatim as keys.
// Arrays are represented by the shape of their first element. nil if not JSON.
func Shape(body []byte) any {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return shapeOf(v, 0)
}

func shapeOf(v any, depth int) any {
	if depth >= maxShapeDepth {
		return "..."
	}
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			switch {
			case IsSecret(k):
				k = "{secret}"
			case isIDKey(k):
				k = "{key}"
			}
			m[k] = shapeOf(e, depth+1)
		}
		return m
	case []any:
		if len(x) == 0 {
			return []any{}
		}
		return []any{shapeOf(x[0], depth+1)}
	case string:
		switch {
		case IsSecret(x):
			return "secret"
		case IsID(x):
			return "id:" + HashID(x)
		default:
			return "string"
		}
	case json.Number:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return "?"
}
