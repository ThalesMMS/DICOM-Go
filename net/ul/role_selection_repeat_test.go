package ul

import (
	"errors"
	"testing"
)

// DCMTK-based acceptors such as Orthanc return one SCP/SCU Role Selection
// item per accepted presentation context, so a class proposed in several
// contexts (one per transfer syntax) comes back repeated in the A-ASSOCIATE-AC.
func TestRoleSelectionsIgnoreIdenticalRepeatsAndRefuseConflicts(t *testing.T) {
	const ct, mr = "1.2.840.10008.5.1.4.1.1.2", "1.2.840.10008.5.1.4.1.1.4"
	proposed := []RoleSelectionItem{{SopClassUID: ct, SCPRole: true}, {SopClassUID: mr, SCPRole: true}}
	accepted, err := acceptedRoleSelectionsFromUserInfo([]UserVariableItem{
		RoleSelectionItem{SopClassUID: ct, SCPRole: true},
		RoleSelectionItem{SopClassUID: ct, SCPRole: true},
		&RoleSelectionItem{SopClassUID: mr, SCPRole: true},
		RoleSelectionItem{SopClassUID: ct, SCPRole: true},
	}, proposed)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 2 || accepted[0] != proposed[0] || accepted[1] != proposed[1] {
		t.Fatalf("accepted roles = %+v, want one per SOP Class in first-seen order", accepted)
	}

	_, err = acceptedRoleSelectionsFromUserInfo([]UserVariableItem{
		RoleSelectionItem{SopClassUID: ct, SCPRole: true},
		RoleSelectionItem{SopClassUID: ct, SCURole: true},
	}, []RoleSelectionItem{{SopClassUID: ct, SCURole: true, SCPRole: true}})
	if !errors.Is(err, ErrInvalidUserItem) {
		t.Fatalf("conflicting repeats: err = %v, want ErrInvalidUserItem", err)
	}
}
