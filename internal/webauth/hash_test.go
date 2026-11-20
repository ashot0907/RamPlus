package webauth

import (
	"strings"
	"testing"
)

var cheap = Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashVerify(t *testing.T) {
	h, err := Hash("correct horse", cheap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("format %q", h)
	}
	if ok, err := Verify(h, "correct horse"); !ok || err != nil {
		t.Errorf("right password: %v %v", ok, err)
	}
	if ok, _ := Verify(h, "correct horsE"); ok {
		t.Error("wrong password accepted")
	}
	h2, _ := Hash("correct horse", cheap)
	if h2 == h {
		t.Error("salt must be random")
	}
}

func TestDefaultParams(t *testing.T) {
	h, err := Hash("password1", DefaultParams)
	if err != nil {
		t.Fatal(err)
	}
	p, salt, key, err := Parse(h)
	if err != nil || p.Memory != 64*1024 || p.Time != 3 || p.Threads != 2 || len(salt) != 16 || len(key) != 32 {
		t.Errorf("%+v %d %d %v", p, len(salt), len(key), err)
	}
}

func TestParseRejects(t *testing.T) {
	good, _ := Hash("password1", cheap)
	parts := strings.Split(good, "$")
	bad := []string{
		"", "plain", "$argon2i$v=19$m=8192,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=16$m=8192,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=1,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=8192,t=99,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=8192,t=1,p=1$!!!$" + parts[5],
		"$argon2id$v=19$m=8192,t=1,p=1$" + parts[4] + "$c2hvcnQ",
	}
	for _, b := range bad {
		if _, _, _, err := Parse(b); err == nil {
			t.Errorf("accepted %q", b)
		}
		if ok, err := Verify(b, "password1"); ok || err == nil {
			t.Errorf("verify %q: %v %v", b, ok, err)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"12345678": true, "пароль-длинный": true, "1234567": false, "        ": false,
		"abc\ndefgh": false, string(make([]byte, 2000)): false, "\xff\xfe12345678": false,
	} {
		if err := CheckPassword(pw); (err == nil) != ok {
			t.Errorf("%q: %v", pw, err)
		}
	}
}

func TestFile(t *testing.T) {
	h, _ := Hash("password1", cheap)
	got, err := ParseFile(FileContent(h))
	if err != nil || got != h {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := ParseFile([]byte("# only comment\n\n")); err == nil {
		t.Error("empty file accepted")
	}
	if _, err := ParseFile([]byte("garbage\n")); err == nil {
		t.Error("garbage accepted")
	}
}
