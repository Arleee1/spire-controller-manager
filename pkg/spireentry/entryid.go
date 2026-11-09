package spireentry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/spiffe/spire-controller-manager/pkg/spireapi"
)

func writeIDField(w io.Writer, value string) {
	_, _ = fmt.Fprintf(w, "%d:%s", len(value), value)
}

func makeEntryKey(entry spireapi.Entry) entryKey {
	h := sha256.New()
	writeIDField(h, entry.SPIFFEID.String())
	writeIDField(h, entry.ParentID.String())
	for _, selector := range slices.Compact(sortSelectors(entry.Selectors)) {
		writeIDField(h, selector.Type)
		writeIDField(h, selector.Value)
	}
	return entryKey(hex.EncodeToString(h.Sum(nil)))
}

func scopedEntryIDPrefix(prefix string, scope ...string) string {
	var data strings.Builder
	for _, field := range scope {
		writeIDField(&data, field)
	}
	return prefix + uuid.NewSHA1(uuid.NameSpaceOID, []byte(data.String())).String() + "."
}

func deterministicEntryID(prefix string, entry spireapi.Entry) string {
	return prefix + uuid.NewSHA1(uuid.NameSpaceOID, []byte(makeEntryKey(entry))).String()
}
