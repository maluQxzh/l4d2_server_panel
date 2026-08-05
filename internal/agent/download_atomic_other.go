//go:build !linux && !windows

package agent

type managedDownloadFile struct{}

func createManagedDownloadFile(string, string, secureWriteOptions) (*managedDownloadFile, error) {
	return nil, errManagedSegmentedDownloadUnsupported
}

func (*managedDownloadFile) WriteAt([]byte, int64) (int, error) {
	return 0, errManagedSegmentedDownloadUnsupported
}
func (*managedDownloadFile) ReadAt([]byte, int64) (int, error) {
	return 0, errManagedSegmentedDownloadUnsupported
}
func (*managedDownloadFile) Truncate(int64) error { return errManagedSegmentedDownloadUnsupported }
func (*managedDownloadFile) Sync() error          { return errManagedSegmentedDownloadUnsupported }
func (*managedDownloadFile) Size() (int64, error) { return 0, errManagedSegmentedDownloadUnsupported }
func (*managedDownloadFile) Commit() error        { return errManagedSegmentedDownloadUnsupported }
func (*managedDownloadFile) Abort()               {}
