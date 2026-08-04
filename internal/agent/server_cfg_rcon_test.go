package agent

import "testing"

func TestEffectiveServerConfigRCONPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		content   string
		want      string
		wantFound bool
		wantError bool
	}{
		{name: "quoted", content: "hostname test\nrcon_password \"correct horse battery staple\"\n", want: "correct horse battery staple", wantFound: true},
		{name: "last assignment wins", content: "rcon_password old\nrcon_password new // active value\n", want: "new", wantFound: true},
		{name: "commented out", content: "// rcon_password secret\nhostname test\n"},
		{name: "empty quoted", content: "rcon_password \"\"\n", wantFound: true},
		{name: "semicolon", content: "hostname test; rcon_password secret; sv_lan 0\n", want: "secret", wantFound: true},
		{name: "escaped quote", content: "rcon_password \"a\\\"b\\\\c\"\n", want: "a\"b\\c", wantFound: true},
		{name: "UTF-8 BOM", content: "\ufeffrcon_password secret\n", want: "secret", wantFound: true},
		{name: "unterminated", content: "rcon_password \"secret\n", wantError: true},
		{name: "NUL", content: "rcon_password secret\x00\n", wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, found, err := effectiveServerConfigRCONPassword([]byte(test.content))
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError=%v", err, test.wantError)
			}
			if got != test.want || found != test.wantFound {
				t.Fatalf("password=%q found=%v, want password=%q found=%v", got, found, test.want, test.wantFound)
			}
		})
	}
}

func TestConstantTimeStringEqual(t *testing.T) {
	t.Parallel()
	if !constantTimeStringEqual("secret", "secret") {
		t.Fatal("equal strings did not compare equal")
	}
	for _, value := range []string{"", "Secret", "secret2"} {
		if constantTimeStringEqual("secret", value) {
			t.Fatalf("different value %q compared equal", value)
		}
	}
}
