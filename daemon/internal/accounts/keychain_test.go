package accounts

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
)

// Touches the real login keychain, with a throwaway item: VINEYARD_KEYCHAIN_TEST=1 go test ./internal/accounts
func TestKeychainRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("VINEYARD_KEYCHAIN_TEST") != "1" {
		t.Skip("set VINEYARD_KEYCHAIN_TEST=1 on macOS")
	}
	k := keychain{service: "Vineyard test item", account: "roundtrip"}
	defer k.Delete()
	if b, err := k.Get(); err != nil || b != nil {
		t.Fatalf("absent item: %q %v", b, err)
	}
	for _, v := range [][]byte{[]byte(`{"a":"1"}`), []byte(`{"a":"` + strings.Repeat("x", 3000) + `"}`)} {
		if err := k.Put(v); err != nil {
			t.Fatal(err)
		}
		got, err := k.Get()
		if err != nil || !bytes.Equal(got, v) {
			t.Fatalf("got %d bytes %v, want %d", len(got), err, len(v))
		}
	}
	if err := k.Delete(); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete(); err != nil {
		t.Fatalf("deleting a missing item: %v", err)
	}
}
