package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (e *Executor) abortBackupUpload(ctx context.Context, job domain.Job, progress ProgressFunc) (map[string]any, error) {
	uploadID := strings.TrimSpace(payloadString(job.Payload, "upload_id"))
	if uploadID == "" {
		return nil, errors.New("backup upload id is required")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	progress(25, "removing abandoned backup upload staging")
	// removeBackupUploadFiles performs the UUID, trusted-directory,
	// regular-file, symlink/reparse-point and containment checks. This operation
	// intentionally has no generic file or shell fallback.
	if err := removeBackupUploadFiles(e.config.DataDir, uploadID); err != nil {
		// Filesystem errors can contain the absolute Agent data path. Keep the
		// terminal job message path-free; retention cleanup can retry later.
		return nil, errors.New("failed to remove abandoned backup upload staging")
	}
	progress(100, "abandoned backup upload staging removed")
	return map[string]any{"upload_id": uploadID}, nil
}
