package main

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// driveLetter recognises a drive-letter mount point ("x:" or "x:\") and
// returns it as Windows lists it, "X:".
func driveLetter(dir string) (string, bool) {
	d := strings.TrimRight(dir, `\/`)
	if len(d) != 2 || d[1] != ':' {
		return "", false
	}
	c := d[0] &^ 0x20 // upper case
	if c < 'A' || c > 'Z' {
		return "", false
	}
	return string([]byte{c, ':'}), true
}

// checkPrivate checks that a reused directory is this user's alone: a real
// directory (not a link or junction), owned by this user (or, created
// elevated, by Administrators), whose access list lets nobody but this
// user, SYSTEM and the Administrators group in. That is what a directory
// under %LOCALAPPDATA% inherits.
func checkPrivate(dir string) error {
	refuse := func(why string) error {
		return fmt.Errorf("%s must be a directory of yours that only you can open (%s); remove it and mount again", dir, why)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return refuse("not a plain directory")
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	trusted, err := wellKnownSIDs(windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	trusted = append(trusted, me)
	if why, err := admitsOnly(dir, trusted, anyAccess); err != nil || why != "" {
		if err != nil {
			return err
		}
		return refuse(why)
	}
	return nil
}

// adminOnly checks that only administrators can change path: it is owned
// by, and grants write access only to, SYSTEM, Administrators or
// TrustedInstaller. That is how WinFsp's files under Program Files are.
func adminOnly(path string) error {
	admins, err := wellKnownSIDs(windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	installer, err := windows.StringToSid(trustedInstallerSID)
	if err != nil {
		return err
	}
	why, err := admitsOnly(path, append(admins, installer), writeAccess)
	if err != nil {
		return err
	}
	if why != "" {
		return fmt.Errorf("%s can be changed by others than administrators (%s)", path, why)
	}
	return nil
}

// trustedInstallerSID is NT SERVICE\TrustedInstaller, which owns Windows'
// own files and often installed programs'.
const trustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

// anyAccess and writeAccess are the access masks admitsOnly looks for.
const (
	anyAccess = ^windows.ACCESS_MASK(0)
	// writeAccess is what lets someone change a file or what a directory
	// holds: write or append data (add a file or subdirectory), delete a
	// child, delete, change the access list or the owner, and the generic
	// rights that include them.
	writeAccess = windows.ACCESS_MASK(0x2 | 0x4 | 0x40 | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_WRITE | windows.GENERIC_ALL)
)

// admitsOnly reports why path's owner or an entry of its access list
// admits someone outside trusted to any of the rights in mask, or "" if
// none does.
func admitsOnly(path string, trusted []*windows.SID, mask windows.ACCESS_MASK) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	// The owner can always change the access list.
	owner, _, err := sd.Owner()
	if err != nil {
		return "", err
	}
	if !anyEqual(owner, trusted) {
		return "owned by " + owner.String(), nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return "", err
	}
	if dacl == nil {
		return "no access list", nil
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return "", err
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies to what is created inside, not to path
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue // only takes access away
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Sprintf("an access entry of type %d", ace.Header.AceType), nil
		}
		if ace.Mask&mask == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !anyEqual(sid, trusted) {
			return "open to " + sid.String(), nil
		}
	}
	return "", nil
}

func wellKnownSIDs(types ...windows.WELL_KNOWN_SID_TYPE) ([]*windows.SID, error) {
	var sids []*windows.SID
	for _, t := range types {
		sid, err := windows.CreateWellKnownSid(t)
		if err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	return sids, nil
}

func anyEqual(sid *windows.SID, set []*windows.SID) bool {
	for _, s := range set {
		if sid.Equals(s) {
			return true
		}
	}
	return false
}

// currentUserSID is the SID of the user this process runs as.
func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}
