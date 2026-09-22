//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The ACL hardening must run icacls from System32 by absolute path (never a
// PATH lookup from an elevated process) and name principals by well-known SID,
// because "SYSTEM"/"Administrators" are localized account names.
func TestIcaclsCommand(t *testing.T) {
	path, args, err := icaclsCommand(`C:\ProgramData\tncd`)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || !strings.EqualFold(filepath.Base(path), "icacls.exe") ||
		!strings.EqualFold(filepath.Base(filepath.Dir(path)), "System32") {
		t.Errorf("icacls path = %q, want absolute System32\\icacls.exe", path)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"*S-1-5-18:(OI)(CI)F", "*S-1-5-32-544:(OI)(CI)F", "/inheritance:r"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}
