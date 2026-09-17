package trace

import "fmt"

type fmtErr string

func (e fmtErr) Error() string { return string(e) }

func errFmt(format string, args ...any) error {
	return fmtErr(fmt.Sprintf(format, args...))
}
