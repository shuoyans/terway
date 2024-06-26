// Code generated by "stringer -type=eniStatus -trimprefix=status"; DO NOT EDIT.

package eni

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[statusInit-0]
	_ = x[statusCreating-1]
	_ = x[statusInUse-2]
	_ = x[statusDeleting-3]
}

const _eniStatus_name = "InitCreatingInUseDeleting"

var _eniStatus_index = [...]uint8{0, 4, 12, 17, 25}

func (i eniStatus) String() string {
	if i < 0 || i >= eniStatus(len(_eniStatus_index)-1) {
		return "eniStatus(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _eniStatus_name[_eniStatus_index[i]:_eniStatus_index[i+1]]
}
