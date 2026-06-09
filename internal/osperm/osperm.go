// Package osperm restricts on-disk files and directories to their owner in a
// way that actually holds on every platform Pingularity targets.
//
// On Unix a plain chmod 0600/0700 is enough. On Windows os.Chmod only toggles
// the read-only bit and writes no NTFS ACL, so a "0600" file is still readable
// by every local account - useless for the key file and the secret-bearing DB.
// SecureFile/SecureDir paper over that: same chmod on Unix, an explicit
// owner-only DACL on Windows.
package osperm
