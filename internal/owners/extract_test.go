package owners_test

import (
	"reflect"
	"testing"

	"github.com/HaiNNT/c-hottag/internal/owners"
)

func TestExtractIDs(t *testing.T) {
	cases := []struct {
		body, field string
		want        []string
	}{
		{`{"session":{"id":"cse_1","other":"x"}}`, "session.id", []string{"cse_1"}},
		{`{"environment_id":"env_1"}`, "environment_id", []string{"env_1"}},
		{`{"slug":"abc"}`, "slug", []string{"abc"}},
		{`{"data":[{"id":"a"},{"id":"b"},{"id":"a"},{"id":7},{"x":1}]}`, "data[].id", []string{"a", "b"}},
		{`{"data":[]}`, "data[].id", nil},
		{`{"session":"flat"}`, "session.id", nil},
		{`{"slug":""}`, "slug", nil},
		{`not json`, "slug", nil},
		{`{"a":{"b":[{"c":["x","y"]}]}}`, "a.b[].c[]", []string{"x", "y"}},
	}
	for _, c := range cases {
		if got := owners.ExtractIDs([]byte(c.body), c.field); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ExtractIDs(%s, %q) = %v, want %v", c.body, c.field, got, c.want)
		}
	}
}
