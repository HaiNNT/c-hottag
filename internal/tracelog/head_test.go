package tracelog_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/HaiNNT/c-hottag/internal/tracelog"
)

func TestNewIDIsEightLowercaseHex(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := tracelog.NewID()
		if !re.MatchString(id) {
			t.Fatalf("NewID() = %q, want 8 lowercase hex characters", id)
		}
		seen[id] = true
	}
	// 100 draws of 32 random bits: a repeat is a ~1e-6 event, so fewer than
	// 99 distinct values means the ids are not random.
	if len(seen) < 99 {
		t.Fatalf("100 NewID calls gave only %d distinct ids", len(seen))
	}
}

// fullRecord sets every Record field to a non-zero value, so HeadOf's
// allowlist is checked field by field.
func fullRecord() tracelog.Record {
	return tracelog.Record{
		T: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), Kind: "req", ID: "0a1b2c3d", SID: "01234567",
		Form: "mitm", Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		PathIDs: []string{"cse_1234abcd"}, QueryKeys: []string{"beta=true"},
		Class: "serving", Auth: "oauth-access", Swapped: true, Account: "B", Drift: true, Refused: 401,
		Unreplayable: true, UnreplayableBytes: 99, OwnerRefused: true,
		Status: 429, Millis: 1234, ReqType: "application/json", RespType: "text/event-stream",
		Upgrade: "websocket", ReqShape: map[string]any{"model": "string"}, RespShape: "string",
		RespHeaderNames: []string{"Retry-After"}, RespLimitHeaders: map[string]string{"retry-after": "30"},
		RespErrorType: "rate_limit_error", RespResetAt: "2026-09-24T11:00:00Z",
		Mark: "m", Err: "aborted mid-body",
	}
}

func TestHeadOfKeepsOnlyHeaderTimeFields(t *testing.T) {
	got := tracelog.HeadOf(fullRecord())
	want := tracelog.Record{
		T: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), Kind: "head", ID: "0a1b2c3d", SID: "01234567",
		Form: "mitm", Method: "POST", Host: "api.anthropic.com", Path: "/v1/messages",
		PathIDs: []string{"cse_1234abcd"}, QueryKeys: []string{"beta=true"},
		Class: "serving", Auth: "oauth-access", Swapped: true, Account: "B", Drift: true, Refused: 401,
		Status: 429, ReqType: "application/json", RespType: "text/event-stream",
		RespHeaderNames: []string{"Retry-After"}, RespLimitHeaders: map[string]string{"retry-after": "30"},
		RespErrorType: "rate_limit_error", RespResetAt: "2026-09-24T11:00:00Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HeadOf:\n got %+v\nwant %+v", got, want)
	}
}

// TestHeadOfClassifiesEveryRecordField fails when Record gains a field this
// list does not name, so whoever adds it decides whether a head record may
// carry it (spec §4.2: the head adds no new category of data to disk).
func TestHeadOfClassifiesEveryRecordField(t *testing.T) {
	inHead := map[string]bool{
		"T": true, "Kind": true, "ID": true, "SID": true, "Form": true, "Method": true, "Host": true, "Path": true,
		"PathIDs": true, "QueryKeys": true, "Class": true, "Auth": true, "Swapped": true,
		"Account": true, "Drift": true, "Refused": true, "Status": true, "ReqType": true, "RespType": true,
		"RespHeaderNames": true, "RespLimitHeaders": true, "RespErrorType": true, "RespResetAt": true,
		"Unreplayable": false, "UnreplayableBytes": false, "Millis": false, "Upgrade": false,
		"ReqShape": false, "RespShape": false, "Mark": false, "Err": false, "OwnerRefused": false,
	}
	head := reflect.ValueOf(tracelog.HeadOf(fullRecord()))
	typ := head.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		keep, ok := inHead[name]
		if !ok {
			t.Errorf("Record.%s is not classified: add it to HeadOf (known at header time) or to this list as false", name)
			continue
		}
		if zero := head.Field(i).IsZero(); zero == keep {
			t.Errorf("Record.%s: in head = %v, want %v", name, !zero, keep)
		}
	}
}

// TestRecordWithoutIDMarshalsAsBefore pins that a record with no ID is
// byte-identical to its encoding before M1c6a: the golden string is what
// json.Marshal produced for this record at 4433ebd.
func TestRecordWithoutIDMarshalsAsBefore(t *testing.T) {
	b, err := json.Marshal(tracelog.Record{
		T: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), Kind: "req", Form: "mitm", Method: "POST",
		Host: "api.anthropic.com", Path: "/v1/messages", Class: "serving", Auth: "oauth-access",
		Status: 200, Millis: 12, RespType: "text/event-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	const golden = `{"t":"2026-09-24T10:00:00Z","kind":"req","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","class":"serving","auth":"oauth-access","status":200,"ms":12,"respType":"text/event-stream"}`
	if string(b) != golden {
		t.Fatalf("got  %s\nwant %s", b, golden)
	}
}

func TestReadAllReadsHeadRecordsAndOldLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	lines := `{"t":"2026-09-24T10:00:00Z","kind":"req","form":"mitm","method":"GET","host":"api.anthropic.com","path":"/v1/models","status":200}
{"t":"2026-09-24T10:00:01Z","kind":"head","id":"0a1b2c3d","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","status":200,"respType":"text/event-stream"}
{"t":"2026-09-24T10:00:01Z","kind":"req","id":"0a1b2c3d","form":"mitm","method":"POST","host":"api.anthropic.com","path":"/v1/messages","status":200,"ms":5000,"respType":"text/event-stream"}
`
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	recs, err := tracelog.ReadAll(p)
	if err != nil || len(recs) != 3 {
		t.Fatalf("recs %+v err %v", recs, err)
	}
	old := tracelog.Record{T: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC), Kind: "req", Form: "mitm", Method: "GET", Host: "api.anthropic.com", Path: "/v1/models", Status: 200}
	if !reflect.DeepEqual(recs[0], old) {
		t.Fatalf("old line read as %+v", recs[0])
	}
	if recs[1].Kind != "head" || recs[1].ID != "0a1b2c3d" || recs[2].Kind != "req" || recs[2].ID != "0a1b2c3d" || recs[2].Millis != 5000 {
		t.Fatalf("head/req pair read as %+v / %+v", recs[1], recs[2])
	}
}

// TestSIDMarshalsAsSidAndIsOmittedWhenEmpty pins the field's wire name and
// that a record with no session (every legacy one) is unchanged on disk.
func TestSIDMarshalsAsSidAndIsOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(tracelog.Record{Kind: "tunnel", SID: "01234567"})
	if err != nil || !strings.Contains(string(b), `"sid":"01234567"`) {
		t.Fatalf("got %s, err %v", b, err)
	}
	b, err = json.Marshal(tracelog.Record{Kind: "tunnel"})
	if err != nil || strings.Contains(string(b), "sid") {
		t.Fatalf("got %s, err %v", b, err)
	}
}
