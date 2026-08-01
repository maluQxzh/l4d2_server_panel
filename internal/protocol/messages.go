package protocol

import (
	"encoding/json"
	"time"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

const (
	Version         = "1.0"
	PreviousVersion = "0.9"
)

// CompatibleVersion deliberately keeps the wire compatibility window narrow:
// a control plane accepts the current protocol and exactly one previous minor
// version, so rolling Agent upgrades do not require node downtime.
func CompatibleVersion(value string) bool {
	return value == Version || value == PreviousVersion
}

const (
	TypeHello     = "hello"
	TypeHeartbeat = "heartbeat"
	TypeMetrics   = "metrics"
	TypeJob       = "job"
	TypeJobCancel = "job_cancel"
	TypeJobAck    = "job_ack"
	TypeJobUpdate = "job_update"
	TypeLog       = "log"
	TypeInventory = "inventory"
)

type Envelope struct {
	Version   string          `json:"version"`
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	NodeID    string          `json:"node_id,omitempty"`
	JobID     string          `json:"job_id,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

func NewEnvelope(messageType, nodeID, jobID string, payload any) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Version: Version, Type: messageType, NodeID: nodeID, JobID: jobID,
		Timestamp: time.Now().UTC(), Payload: raw,
	}, nil
}

type Hello struct {
	Name         string   `json:"name"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	AgentVersion string   `json:"agent_version"`
	InstanceRoot string   `json:"instance_root"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type Heartbeat struct {
	AgentVersion string            `json:"agent_version"`
	Instances    map[string]string `json:"instances"`
}

type JobCommand struct {
	Job domain.Job `json:"job"`
}

type JobUpdate struct {
	Status   domain.JobStatus `json:"status"`
	Progress int              `json:"progress"`
	Message  string           `json:"message,omitempty"`
	Result   map[string]any   `json:"result,omitempty"`
}

type LogEvent struct {
	InstanceID string    `json:"instance_id"`
	Stream     string    `json:"stream"`
	Line       string    `json:"line"`
	At         time.Time `json:"at"`
}
