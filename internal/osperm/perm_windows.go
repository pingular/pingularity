//go:build windows

package osperm

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// A PROTECTED (PAI) DACL granting full control to the current user (the process
// account, named by its explicit SID), SYSTEM, and the local Administrators
// group, with inheritance disabled so a permissive parent directory can't widen
// access. This is the Windows analogue of Unix 0600/0700: no entry for
// Users/Everyone means non-admin accounts get nothing.
//
// The account must be named by SID, not the OWNER-RIGHTS well-known SID ("OW"):
// OW is inert unless the token carries the owner-rights privilege, so a DACL
// built with it locks a non-elevated process out of its own data dir - the DB
// then fails to open (SQLITE_CANTOPEN). Service (SYSTEM) and admin (BA) runs
// still worked, which is why this went unnoticed until a plain user run.
const ownerOnlySDDLFmt = "D:PAI(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)"

// SecureFile applies the owner-only DACL to a single file.
func SecureFile(path string) error { return secure(path) }

// GroupOrWorldAccessible can't be judged from Unix mode bits on Windows: access
// is governed by the DACL SecureFile sets, not by the synthetic perms os.Stat
// reports. Report "unknown" so the caller's verify step is a no-op here.
func GroupOrWorldAccessible(path string) (accessible, known bool) { return false, false }

// SecureDir applies the owner-only DACL to a directory. New children inherit
// nothing (inheritance is off), so each secured file also sets its own DACL.
func SecureDir(path string) error { return secure(path) }

func secure(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("osperm: current user SID: %w", err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf(ownerOnlySDDLFmt, sid))
	if err != nil {
		return fmt.Errorf("osperm: parse DACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("osperm: read DACL: %w", err)
	}
	// PROTECTED_DACL_SECURITY_INFORMATION makes the DACL authoritative (drops
	// inherited ACEs); DACL_SECURITY_INFORMATION says we're setting the DACL.
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("osperm: set DACL on %s: %w", path, err)
	}
	return nil
}

// currentUserSID returns the SID string of the account this process runs as
// (the service's LocalSystem, an admin, or a plain user), for naming in the DACL.
func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}
