package cmd

import "testing"

func TestWebInsecureGuard(t *testing.T) {
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "", "", "", false); err == nil {
		t.Fatal("expected unauthenticated unspecified bind to fail")
	}
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "", "", "", true); err != nil {
		t.Fatalf("--insecure rejected: %v", err)
	}
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "token", "", "", false); err != nil {
		t.Fatalf("authenticated bind rejected: %v", err)
	}
	// Form login counts as authentication: a public bind with --auth-login is
	// exactly what a self-hosted deployment needs.
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "", "", "admin:hunter2hunter2", false); err != nil {
		t.Fatalf("--auth-login bind rejected: %v", err)
	}
	// Loopback stays open without credentials: local-only use must not need a flag.
	if err := validateArchiveServeSecurity("127.0.0.1:18091", "", "", "", false); err != nil {
		t.Fatalf("loopback bind rejected: %v", err)
	}
}

func TestWebAuthModesAreMutuallyExclusive(t *testing.T) {
	cases := [][3]string{
		{"token", "admin:pass", ""},
		{"token", "", "admin:pass"},
		{"", "admin:pass", "admin:pass"},
		{"token", "admin:pass", "admin:pass"},
	}
	for _, c := range cases {
		if err := validateArchiveServeSecurity("0.0.0.0:18091", c[0], c[1], c[2], false); err == nil {
			t.Fatalf("token=%q basic=%q login=%q: expected mutual-exclusion error", c[0], c[1], c[2])
		}
	}
}

// An empty Basic password authenticates anyone who sends the empty string, so
// the flag parser must refuse it rather than start an "authenticated" server.
func TestParseAuthPairRejectsEmptyPassword(t *testing.T) {
	if _, _, err := parseAuthPair("--auth-basic", "admin:"); err == nil {
		t.Fatal("empty password accepted")
	}
	if _, _, err := parseAuthPair("--auth-basic", ":pass"); err == nil {
		t.Fatal("empty username accepted")
	}
	if _, _, err := parseAuthPair("--auth-basic", "admin"); err == nil {
		t.Fatal("missing colon accepted")
	}
	user, pass, err := parseAuthPair("--auth-basic", "admin:sec ret")
	if err != nil || user != "admin" || pass != "sec ret" {
		t.Fatalf("user=%q pass=%q err=%v", user, pass, err)
	}
}

// A hostname is resolved: "myhost:8080" must not bypass the public-bind guard.
func TestLoopbackHost(t *testing.T) {
	if ok, err := loopbackHost("127.0.0.1"); err != nil || !ok {
		t.Fatalf("127.0.0.1: ok=%v err=%v", ok, err)
	}
	if ok, _ := loopbackHost("0.0.0.0"); ok {
		t.Fatal("0.0.0.0 reported as loopback")
	}
	if ok, err := loopbackHost("localhost"); err != nil || !ok {
		t.Fatalf("localhost: ok=%v err=%v", ok, err)
	}
}
