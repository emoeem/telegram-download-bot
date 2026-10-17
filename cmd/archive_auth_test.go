package cmd

import "testing"

func TestWebInsecureGuard(t *testing.T) {
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "", "", false); err == nil {
		t.Fatal("expected unauthenticated unspecified bind to fail")
	}
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "", "", true); err != nil {
		t.Fatalf("--insecure rejected: %v", err)
	}
	if err := validateArchiveServeSecurity("0.0.0.0:18091", "token", "", false); err != nil {
		t.Fatalf("authenticated bind rejected: %v", err)
	}
}
