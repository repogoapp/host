package envsource

import (
	"reflect"
	"testing"
)

// The cases v1's Swift and TypeScript parsers agree on; the phone may still
// hold a copy of either, so this one must too.
func TestParseMatchesV1(t *testing.T) {
	text := "# comment\n" +
		"\n" +
		"PLAIN=value\n" +
		"export EXPORTED=yes\n" +
		"  SPACED  =  padded  \n" +
		`DOUBLE="quoted value"` + "\n" +
		`SINGLE='single'` + "\n" +
		`MISMATCHED="open'` + "\n" +
		`LONE="` + "\n" +
		`ESCAPED="a\nb"` + "\n" +
		"EQUALS=a=b=c\n" +
		"EMPTY=\n" +
		"CRLF=windows\r\n" +
		"=novalue\n" +
		"no equals sign\n" +
		"PLAIN=later wins\n"
	want := map[string]string{
		"PLAIN":      "later wins",
		"EXPORTED":   "yes",
		"SPACED":     "padded",
		"DOUBLE":     "quoted value",
		"SINGLE":     "single",
		"MISMATCHED": `"open'`,
		"LONE":       `"`,
		"ESCAPED":    `a\nb`,
		"EQUALS":     "a=b=c",
		"EMPTY":      "",
		"CRLF":       "windows",
	}
	if got := Parse(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse =\n%v\nwant\n%v", got, want)
	}
}
