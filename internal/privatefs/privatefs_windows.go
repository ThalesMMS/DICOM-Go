//go:build windows

package privatefs

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func protect(path string) error {
	// Windows os.Chmod only uses the owner-write bit. Setting Mode here clears
	// FILE_ATTRIBUTE_READONLY before the DACL is replaced.
	if err := os.Chmod(path, 0600); err != nil {
		return err
	}
	userSID, systemSID, err := privacySIDs()
	if err != nil {
		return err
	}
	sids := []*windows.SID{userSID}
	if !userSID.Equals(systemSID) {
		sids = append(sids, systemSID)
	}

	var pinner runtime.Pinner
	defer pinner.Unpin()
	access := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		pinner.Pin(sid)
		access = append(access, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		userSID,
		nil,
		acl,
		nil,
	)
}

func isPrivate(path string, _ os.FileInfo) (bool, error) {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return false, err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return false, nil
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return false, err
	}
	if dacl == nil {
		return false, nil
	}

	userSID, systemSID, err := privacySIDs()
	if err != nil {
		return false, err
	}
	ownerSID, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	if ownerSID == nil || !ownerSID.Equals(userSID) {
		return false, nil
	}
	ownerRightsSID, err := windows.StringToSid("S-1-3-4")
	if err != nil {
		return false, err
	}
	want := map[string]bool{userSID.String(): false}
	if !userSID.Equals(systemSID) {
		want[systemSID.String()] = false
	}
	if int(dacl.AceCount) < 1 || int(dacl.AceCount) > len(want) {
		return false, nil
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return false, err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE != 0 || !hasFullFileAccess(ace.Mask) {
			return false, nil
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(ownerRightsSID) {
			sid = userSID
		}
		key := sid.String()
		seen, allowed := want[key]
		if !allowed || seen {
			return false, nil
		}
		want[key] = true
	}
	for key, seen := range want {
		if !seen && key == userSID.String() {
			return false, nil
		}
	}
	return true, nil
}

func privacySIDs() (*windows.SID, *windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, err
	}
	userSID, err := user.User.Sid.Copy()
	if err != nil {
		return nil, nil, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, nil, err
	}
	return userSID, systemSID, nil
}

func hasFullFileAccess(mask windows.ACCESS_MASK) bool {
	const fileAllAccess windows.ACCESS_MASK = windows.STANDARD_RIGHTS_REQUIRED |
		windows.SYNCHRONIZE | windows.ACCESS_MASK(0x1ff)
	return mask&windows.GENERIC_ALL != 0 || mask&fileAllAccess == fileAllAccess
}
