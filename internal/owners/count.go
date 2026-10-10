package owners

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

// ErrCorrupt is CountByAccount's answer for a file that is not an owners
// map. The file is left as it is.
var ErrCorrupt = errors.New("owners: the file is not valid owners JSON")

// CountByAccount reads path without locking and without writing anything,
// and counts its entries per account name, spelled as stored. A missing
// file is an empty map. It is doctor's read-only view (M2 spec §2.2 row
// 12). Open would start a writer, and Edit takes the lock and moves a
// corrupt file aside. Writers replace the file by atomic rename, so an
// unlocked read sees one whole version.
func CountByAccount(path string) (map[string]int, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]entry
	if json.Unmarshal(b, &m) != nil {
		return nil, ErrCorrupt
	}
	counts := make(map[string]int)
	for k, e := range m {
		if isListerKey(k) {
			continue // a connector lister row, not an object
		}
		counts[e.Account]++
	}
	return counts, nil
}
