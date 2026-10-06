package postgres

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestLedgerCursorWalletAndOrdering(t *testing.T) {
	original := ledgerCursor{"w", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "id"}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(data)
	decoded, err := decodeLedgerCursor(token, "w")
	if err != nil || decoded != original {
		t.Fatal(decoded, err)
	}
	for _, test := range []struct{ token, wallet string }{{token, "other"}, {"invalid", "w"}, {base64.RawURLEncoding.EncodeToString([]byte(`{}`)), "w"}} {
		if _, err := decodeLedgerCursor(test.token, test.wallet); err == nil {
			t.Fatal("accepted invalid cursor")
		}
	}
}
