package license

// Build-time injected verifier material.
//
// These are plain string variables rather than constants so `-ldflags -X` can
// set them. They are empty in a source build, which means the licence gate is
// simply absent there (`tdl archive serve` behaves exactly as before) — a
// developer running the tree should not have to hold the vendor's master key.
//
//	-X github.com/iyear/tdl/license.hmacKeyB64=<base64>
//	-X github.com/iyear/tdl/license.pubKeyB64=<base64>
//
// The release build passes both; see `tdl license verifier`.
var (
	hmacKeyB64 = ""
	pubKeyB64  = ""
)

// BuildVerifier returns the verifier this binary was compiled with.
//
// A build with neither value yields a Verifier whose Configured() is false, and
// every gate fails closed except when the caller explicitly treats an
// unconfigured build as "licensing not enabled" — which is the decision made in
// archive.NewLicenceGate.
func BuildVerifier() (*Verifier, error) {
	return NewVerifier(hmacKeyB64, pubKeyB64)
}

// Injected reports whether this binary carries any verifier material.
func Injected() bool {
	return hmacKeyB64 != "" || pubKeyB64 != ""
}
