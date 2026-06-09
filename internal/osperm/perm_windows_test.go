//go:build windows

package osperm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSecureFileDACLWindows verifies the EFFECTIVE DACL a secured file ends up
// with, not just that SecureFile returned nil - the gap that let a prior
// owner-rights DACL bug (see perm_windows.go) reach a plain-user run. It reads
// the DACL back and asserts three things: no broad principal (Everyone / built-in
// Users / Authenticated Users) is granted anything, the DACL is PROTECTED so a
// permissive parent can't re-widen it, and the owning account keeps access (a
// DACL that locks the process out of its own file fails SQLITE_CANTOPEN). Exact
// ACE counts are deliberately NOT asserted - they vary with legitimate Windows
// ACL representations.
func TestSecureFileDACLWindows(t *testing.T) {
	f := filepath.Join(t.TempDir(), "pingularity.key")
	if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := SecureFile(f); err != nil {
		t.Fatalf("SecureFile: %v", err)
	}

	sd, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	sddl := sd.String()

	// Broad principals in SDDL trustee position: Everyone (WD / S-1-1-0), the
	// built-in Users group (BU / S-1-5-32-545), Authenticated Users (AU /
	// S-1-5-11). An allow ACE ends "...;<trustee>)".
	for _, broad := range []string{";WD)", ";BU)", ";AU)", ";S-1-1-0)", ";S-1-5-32-545)", ";S-1-5-11)"} {
		if strings.Contains(sddl, broad) {
			t.Errorf("secured file DACL grants a broad principal (%s): %s", broad, sddl)
		}
	}

	// The DACL flags (between "D:" and the first ACE) must include 'P' (protected),
	// so inherited ACEs from a permissive parent are dropped.
	if i := strings.Index(sddl, "D:"); i >= 0 {
		flags := sddl[i+2:]
		if j := strings.IndexByte(flags, '('); j >= 0 {
			flags = flags[:j]
		}
		if !strings.Contains(flags, "P") {
			t.Errorf("secured file DACL is not protected (flags %q): %s", flags, sddl)
		}
	} else {
		t.Errorf("secured file has no DACL in its security descriptor: %s", sddl)
	}

	// The owning account must retain access. Match the owner SID the way Windows
	// renders it in a DACL, not the raw SID string: Windows canonicalizes some
	// SIDs to SDDL aliases (e.g. the built-in Administrator that CI runs as shows
	// as "LA"), so a raw substring match on the SID spuriously fails there even
	// though SecureFile granted that account. Round-trip the owner SID through the
	// same SDDL rendering, then look for that trustee token.
	if owner, err := currentUserSID(); err == nil {
		tok := owner
		if probe, perr := windows.SecurityDescriptorFromString("D:(A;;FA;;;" + owner + ")"); perr == nil {
			s := probe.String() // e.g. "D:(A;;FA;;;LA)" or "D:(A;;FA;;;S-1-5-21-...)"
			if a, b := strings.LastIndex(s, ";"), strings.LastIndex(s, ")"); a >= 0 && b > a {
				tok = s[a+1 : b]
			}
		}
		if !strings.Contains(sddl, ";"+tok+")") {
			t.Errorf("secured file DACL drops the owning account %s (rendered %q): %s", owner, tok, sddl)
		}
	}
}
