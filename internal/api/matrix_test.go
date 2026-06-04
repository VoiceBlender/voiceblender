package api

import (
	"context"
	"strings"
	"testing"

	"maunium.net/go/mautrix/id"
)

// TestParseMatrixDestination covers the local-only branches of the parser
// (raw room id, matrix:roomid/... URI, empty input, malformed input). Alias
// resolution branches need a real homeserver and live in integration tests.
func TestParseMatrixDestination(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    id.RoomID
		errLike string
	}{
		{name: "empty", input: "", errLike: "'to' is required"},
		{name: "whitespace", input: "   ", errLike: "'to' is required"},
		{name: "raw room id", input: "!abc:example.org", want: "!abc:example.org"},
		{name: "matrix uri roomid", input: "matrix:roomid/abc:example.org", want: "!abc:example.org"},
		{name: "matrix uri roomid with query", input: "matrix:roomid/abc:example.org?via=other.org", want: "!abc:example.org"},
		{name: "matrix uri unsupported scheme", input: "matrix:user/alice:example.org", errLike: "unsupported matrix URI"},
		{name: "plain mxid rejected", input: "@alice:example.org", errLike: "invalid matrix destination"},
		{name: "garbage rejected", input: "sip:bob@example.com", errLike: "invalid matrix destination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMatrixDestination(context.Background(), "", "", "", tc.input)
			if tc.errLike != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got room=%q", tc.errLike, got)
				}
				if !strings.Contains(err.Error(), tc.errLike) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.errLike)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got room %q, want %q", got, tc.want)
			}
		})
	}
}
