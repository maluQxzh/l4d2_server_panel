package agent

import (
	"errors"
	"fmt"
	"os"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const generatedConfigRelative = "left4dead2/cfg/l4d2panel.cfg"

type restoreRollbackState struct {
	record              InstanceRecord
	instancePath        string
	generatedConfig     []byte
	generatedConfigSeen bool
}

func captureRestoreRollbackState(record InstanceRecord, instancePath string) (restoreRollbackState, error) {
	state := restoreRollbackState{record: record, instancePath: instancePath}
	raw, err := readRegularFileWithinNoFollow(instancePath, generatedConfigRelative, maxManagedFileSize)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.generatedConfig = raw
	state.generatedConfigSeen = true
	return state, nil
}

func (e *Executor) applyBackupLaunch(record InstanceRecord, launch BackupLaunchConfig, payload operationPayload) (InstanceRecord, string, error) {
	if err := validateBackupLaunchConfig(launch); err != nil {
		return record, "", err
	}
	port := record.RCONPort
	if port == 0 {
		port = payload.Instance.Port
	}
	if port == 0 {
		port = 27015
	}
	instance := domain.Instance{
		ID: record.ID, Name: record.Name, Host: record.RCONHost, Port: port, TVPort: record.TVPort,
		Config: launch.domainConfig(),
	}
	return e.applyConfiguration(record, operationPayload{
		Instance: instance, RCONPassword: payload.RCONPassword, ServerPassword: payload.ServerPassword,
	})
}

func (e *Executor) restoreFailureWithState(job domain.Job, checkpointPath, stage string, cause error, state restoreRollbackState) error {
	var recoveryFailures []error
	if checkpointPath != "" {
		if err := e.rollbackServerExtension(job, checkpointPath, false); err != nil {
			recoveryFailures = append(recoveryFailures, err)
		}
	}
	if err := e.restoreCapturedInstanceState(state); err != nil {
		recoveryFailures = append(recoveryFailures, err)
	}
	if joined := errors.Join(recoveryFailures...); joined != nil {
		return fmt.Errorf("%s failed: %w; pre-restore checkpoint/state rollback also failed: %v", stage, cause, joined)
	}
	return fmt.Errorf("%s failed; pre-restore checkpoint and instance state recovered: %w", stage, cause)
}

func (e *Executor) restoreCapturedInstanceState(state restoreRollbackState) error {
	var failures []error
	if err := e.registry.Put(state.record); err != nil {
		failures = append(failures, fmt.Errorf("restore instance registry: %w", err))
	}
	if state.generatedConfigSeen {
		if err := writeGeneratedServerConfig(state.instancePath, state.generatedConfig); err != nil {
			failures = append(failures, fmt.Errorf("restore generated configuration: %w", err))
		}
	} else {
		if removeErr := deleteManagedFileNoFollow(state.instancePath, generatedConfigRelative); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("remove newly generated configuration during rollback: %w", removeErr))
		}
	}
	return errors.Join(failures...)
}

// writeGeneratedServerConfig is the privileged internal counterpart to the
// public file manager. File jobs deliberately reject this secret-bearing path;
// only lifecycle/configuration code can call this atomic writer.
func writeGeneratedServerConfig(instancePath string, raw []byte) error {
	if len(raw) > maxManagedFileSize {
		return errors.New("generated configuration exceeds the 8 MiB limit")
	}
	return writeRawFileWithinNoFollow(instancePath, generatedConfigRelative, raw, 0o600, true)
}
