// SPDX-License-Identifier: MIT

package recover

import (
	"syscall"
)

// systemDiskProbe is the production DiskProbe, byte-for-byte the same
// discipline as internal/doctor's (syscall.Statfs on macOS/linux; the
// platformScope note there applies): free-bytes answer for the
// filesystem holding path. Reused TYPE is doctor.DiskProbe; a local
// implementation stands in because doctor's is unexported.
//
// DEDUPLICATION TODO (M5, T-503): internal/doctor and internal/recover
// each carry one 8-line statfs read. When a third consumer appears,
// promote `statfs free bytes` into internal/system (or into
// internal/doctor/exported) so both tools call one implementation; until
// then the duplication is deliberate to keep the recovery path's
// import set minimal (ADR-0005).
func systemDiskProbe(path string) (freeBytes uint64, err error) {
	var st syscall.Statfs_t
	if statfsErr := syscall.Statfs(path, &st); statfsErr != nil {
		return 0, statfsErr
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
