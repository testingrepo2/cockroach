// Code generated by "stringer"; DO NOT EDIT.

package pgdate

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[fieldYear-0]
	_ = x[fieldMonth-1]
	_ = x[fieldDay-2]
	_ = x[fieldEra-3]
	_ = x[fieldHour-4]
	_ = x[fieldMinute-5]
	_ = x[fieldSecond-6]
	_ = x[fieldNanos-7]
	_ = x[fieldMeridian-8]
	_ = x[fieldTZHour-9]
	_ = x[fieldTZMinute-10]
	_ = x[fieldTZSecond-11]
	_ = x[fieldMinimum-0]
	_ = x[fieldMaximum-11]
}

func (i field) String() string {
	switch i {
	case fieldYear:
		return "fieldYear"
	case fieldMonth:
		return "fieldMonth"
	case fieldDay:
		return "fieldDay"
	case fieldEra:
		return "fieldEra"
	case fieldHour:
		return "fieldHour"
	case fieldMinute:
		return "fieldMinute"
	case fieldSecond:
		return "fieldSecond"
	case fieldNanos:
		return "fieldNanos"
	case fieldMeridian:
		return "fieldMeridian"
	case fieldTZHour:
		return "fieldTZHour"
	case fieldTZMinute:
		return "fieldTZMinute"
	case fieldTZSecond:
		return "fieldTZSecond"
	default:
		return "field(" + strconv.FormatInt(int64(i), 10) + ")"
	}
}
