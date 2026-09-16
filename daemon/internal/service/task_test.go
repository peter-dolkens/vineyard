package service

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestTaskXMLIsUTF16ForTheGivenUser(t *testing.T) {
	b := taskXML(`C:\Users\Peter Dolkens\.vineyard\bin\vineyardd.exe`, `HAWK\Peter Dolkens`)
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatal("missing UTF-16LE BOM")
	}
	if len(b)%2 != 0 {
		t.Fatal("odd byte length")
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 2; i < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	s := string(utf16.Decode(u))
	for _, want := range []string{`encoding="UTF-16"`, `<UserId>HAWK\Peter Dolkens</UserId>`, `<Command>C:\Users\Peter Dolkens\.vineyard\bin\vineyardd.exe</Command>`, `<Arguments>run</Arguments>`, `<LogonType>InteractiveToken</LogonType>`, `<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	if !strings.Contains(string(taskXML("x", `a<b&"c"`)), string([]byte{'&', 0, 'l', 0, 't', 0, ';', 0})) {
		t.Fatal("username not XML-escaped")
	}
}
