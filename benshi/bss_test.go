package benshi

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// BSS records captured from a UV-PRO on 2026-10-01, produced by toggling the
// radio's "Digital Mode -> Enable" control. The tail is the operator's APRS
// symbol and callsign ("/[KU0HN"), which is what makes a partial rebuild of this
// record destructive rather than merely untidy.
const (
	bssAPRSOn  = "001c801e000000004150525300000000000000000000000000000000000000000000000000002f5b4b5530484e00"
	bssAPRSOff = "000c801e000000004150525300000000000000000000000000000000000000000000000000002f5b4b5530484e00"
)

func mustBSS(t *testing.T, h string) BSSRec {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad fixture %q: %v", h, err)
	}
	rec, err := ParseBSSRec(append([]byte{0x00}, b...))
	if err != nil {
		t.Fatalf("ParseBSSRec(%s): %v", h, err)
	}
	return rec
}

// TestBSSAPRSEnabledMatchesCapturedRecords: the radio produced both of these, so
// matching them byte for byte pins the bit's position and nothing else.
func TestBSSAPRSEnabledMatchesCapturedRecords(t *testing.T) {
	on, off := mustBSS(t, bssAPRSOn), mustBSS(t, bssAPRSOff)
	if !on.APRSEnabled() {
		t.Error("the APRS-on record reads as disabled")
	}
	if off.APRSEnabled() {
		t.Error("the APRS-off record reads as enabled")
	}
	got := on.WithAPRSEnabled(false).Bytes()
	want, _ := hex.DecodeString(bssAPRSOff)
	if !bytes.Equal(got, want) {
		t.Errorf("WithAPRSEnabled(false):\n got  %s\n want %s", hex.EncodeToString(got), bssAPRSOff)
	}
	back := off.WithAPRSEnabled(true).Bytes()
	wantOn, _ := hex.DecodeString(bssAPRSOn)
	if !bytes.Equal(back, wantOn) {
		t.Errorf("WithAPRSEnabled(true):\n got  %s\n want %s", hex.EncodeToString(back), bssAPRSOn)
	}
}

// TestBSSPatchPreservesCallsignAndSymbol is the specific disaster
// read-modify-write exists to prevent here. Asserted on the bytes rather than
// through an accessor, because this package deliberately does not model the
// callsign field -- it only has to not destroy it.
func TestBSSPatchPreservesCallsignAndSymbol(t *testing.T) {
	const ident = "/[KU0HN"
	rec := mustBSS(t, bssAPRSOn)
	if !strings.Contains(string(rec.Bytes()), ident) {
		t.Fatalf("fixture does not contain %q -- test proves nothing", ident)
	}
	if got := string(rec.WithAPRSEnabled(false).Bytes()); !strings.Contains(got, ident) {
		t.Errorf("clearing the APRS bit destroyed %q", ident)
	}
	// And the destination field the record also carries.
	if got := string(rec.WithAPRSEnabled(false).Bytes()); !strings.Contains(got, "APRS") {
		t.Error("clearing the APRS bit destroyed the destination field")
	}
}

// TestBSSPatchIsIdentityWhenUnchanged: restore compares records byte for byte,
// so a no-op patch must be exactly that.
func TestBSSPatchIsIdentityWhenUnchanged(t *testing.T) {
	for _, h := range []string{bssAPRSOn, bssAPRSOff} {
		rec := mustBSS(t, h)
		if got := rec.WithAPRSEnabled(rec.APRSEnabled()); !got.Equal(rec) {
			t.Errorf("WithAPRSEnabled(current) changed %s -> %s", h, hex.EncodeToString(got.Bytes()))
		}
	}
}

// TestBSSPatchDoesNotAliasTheOriginal: the pre-session record is held for the
// length of the session and compared against later.
func TestBSSPatchDoesNotAliasTheOriginal(t *testing.T) {
	orig := mustBSS(t, bssAPRSOn)
	snapshot := orig.Bytes()
	_ = orig.WithAPRSEnabled(false)
	if !bytes.Equal(orig.Bytes(), snapshot) {
		t.Error("patching mutated the original record")
	}
}

// TestParseBSSRecRejectsBadReplies: a rejected or truncated reply must not become
// a record something later writes back over the operator's APRS identity.
func TestParseBSSRecRejectsBadReplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"non-zero status", append([]byte{0x01}, make([]byte, 46)...)},
		{"too short", []byte{0x00, 0x00}},
	} {
		if _, err := ParseBSSRec(tc.body); err == nil {
			t.Errorf("ParseBSSRec(%s) accepted a bad reply", tc.name)
		}
	}
}
