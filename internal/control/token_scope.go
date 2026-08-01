package control

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func (p Principal) HasTokenResourceRestriction() bool {
	return p.IsAPIToken() && (p.TokenNodeID != "" || p.TokenInstanceID != "")
}

func (p Principal) HasResourceRestriction() bool {
	if p.HasTokenResourceRestriction() {
		return true
	}
	if len(p.Bindings) == 0 && !p.BindingsLoaded {
		return false
	}
	if len(p.Bindings) == 0 {
		return true
	}
	for _, binding := range p.Bindings {
		if binding.ScopeType == "global" {
			return false
		}
	}
	return true
}

func (p Principal) AllowsNode(nodeID string) bool {
	userAllowed := len(p.Bindings) == 0 && !p.BindingsLoaded
	for _, binding := range p.Bindings {
		if binding.ScopeType == "global" || (binding.ScopeType == "node" && binding.NodeID == nodeID) {
			userAllowed = true
			break
		}
	}
	if !userAllowed {
		return false
	}
	if !p.HasTokenResourceRestriction() {
		return true
	}
	// An instance-only token does not implicitly gain node-management access.
	if p.TokenInstanceID != "" {
		return false
	}
	return p.TokenNodeID == nodeID
}

func (p Principal) AllowsInstance(instance domain.Instance) bool {
	userAllowed := len(p.Bindings) == 0 && !p.BindingsLoaded
	for _, binding := range p.Bindings {
		if binding.ScopeType == "global" || (binding.ScopeType == "node" && binding.NodeID == instance.NodeID) ||
			(binding.ScopeType == "instance" && binding.InstanceID == instance.ID) {
			userAllowed = true
			break
		}
	}
	if !userAllowed {
		return false
	}
	if !p.HasTokenResourceRestriction() {
		return true
	}
	if p.TokenInstanceID != "" && p.TokenInstanceID != instance.ID {
		return false
	}
	if p.TokenNodeID != "" && p.TokenNodeID != instance.NodeID {
		return false
	}
	return true
}

func (p Principal) AllowsJob(job domain.Job) bool {
	if job.InstanceID != "" {
		return p.AllowsInstance(domain.Instance{ID: job.InstanceID, NodeID: job.NodeID})
	}
	return p.AllowsNode(job.NodeID)
}

func (p Principal) AllowsNodePermission(nodeID string, permission Permission) bool {
	if !p.AllowsNode(nodeID) || (p.IsAPIToken() && len(p.TokenScopes) > 0 && !tokenScopesAllow(p.TokenScopes, permission)) {
		return false
	}
	if len(p.Bindings) == 0 && !p.BindingsLoaded {
		if p.User.Role == "" {
			return true
		}
		return RoleAllows(p.User.Role, permission)
	}
	for _, binding := range p.Bindings {
		if (binding.ScopeType == "global" || (binding.ScopeType == "node" && binding.NodeID == nodeID)) &&
			RoleAllows(binding.Role, permission) {
			return true
		}
	}
	return false
}

func (p Principal) AllowsInstancePermission(instance domain.Instance, permission Permission) bool {
	if !p.AllowsInstance(instance) || (p.IsAPIToken() && len(p.TokenScopes) > 0 && !tokenScopesAllow(p.TokenScopes, permission)) {
		return false
	}
	if len(p.Bindings) == 0 && !p.BindingsLoaded {
		if p.User.Role == "" {
			return true
		}
		return RoleAllows(p.User.Role, permission)
	}
	for _, binding := range p.Bindings {
		matches := binding.ScopeType == "global" || (binding.ScopeType == "node" && binding.NodeID == instance.NodeID) ||
			(binding.ScopeType == "instance" && binding.InstanceID == instance.ID)
		if matches && RoleAllows(binding.Role, permission) {
			return true
		}
	}
	return false
}

func (p Principal) AllowsJobPermission(job domain.Job, permission Permission) bool {
	if job.InstanceID != "" {
		return p.AllowsInstancePermission(domain.Instance{ID: job.InstanceID, NodeID: job.NodeID}, permission)
	}
	return p.AllowsNodePermission(job.NodeID, permission)
}

func (p Principal) AllowsInstanceAction(action string) bool {
	permission, ok := genericInstanceActionPermission(action)
	return ok && p.Allows(permission)
}

// eventAllowed prevents a resource-scoped session or API token from learning
// about unrelated resources through replayed or live WebSocket events. It is
// deliberately database-free: high-volume log and metric frames must not turn
// each subscriber into a PostgreSQL query multiplier. Producers attach the
// node/instance scope to compact events; old or malformed events without enough
// scope information fail closed for restricted principals.
func (s *Server) eventAllowed(_ context.Context, principal Principal, event Event) bool {
	if !principal.HasResourceRestriction() {
		return true
	}
	switch event.ResourceType {
	case "node":
		if event.ResourceID == "" {
			return false
		}
		if instanceID := eventMetricInstanceID(event); instanceID != "" {
			return principal.AllowsInstancePermission(domain.Instance{ID: instanceID, NodeID: event.ResourceID}, PermissionRead)
		}
		return principal.AllowsNodePermission(event.ResourceID, PermissionRead)
	case "instance":
		if instance, ok := eventInstance(event); ok {
			if instance.ID == "" || (event.ResourceID != "" && instance.ID != event.ResourceID) {
				return false
			}
			return principal.AllowsInstancePermission(instance, PermissionRead)
		}
		instanceID := event.ResourceID
		if payloadID := eventString(event.Data, "instance_id"); payloadID != "" {
			if instanceID != "" && payloadID != instanceID {
				return false
			}
			instanceID = payloadID
		}
		if instanceID == "" {
			return false
		}
		return principal.AllowsInstancePermission(domain.Instance{ID: instanceID, NodeID: eventString(event.Data, "node_id")}, PermissionRead)
	case "job":
		if job, ok := eventJob(event); ok {
			if job.ID == "" || (event.ResourceID != "" && job.ID != event.ResourceID) || (job.NodeID == "" && job.InstanceID == "") {
				return false
			}
			return principal.AllowsJobPermission(job, PermissionRead)
		}
		jobID := eventString(event.Data, "job_id")
		if jobID != "" && event.ResourceID != "" && jobID != event.ResourceID {
			return false
		}
		job := domain.Job{ID: event.ResourceID, NodeID: eventString(event.Data, "node_id"), InstanceID: eventString(event.Data, "instance_id")}
		if job.NodeID == "" && job.InstanceID == "" {
			return false
		}
		return principal.AllowsJobPermission(job, PermissionRead)
	default:
		// Global events and newly introduced resource classes are hidden until
		// their ownership relationship is explicitly defined here.
		return false
	}
}

func eventMetricInstanceID(event Event) string {
	raw, ok := event.Data["sample"]
	if !ok {
		return ""
	}
	if sample, ok := raw.(domain.MetricSample); ok {
		return sample.InstanceID
	}
	if values, ok := raw.(map[string]any); ok {
		return eventString(values, "instance_id")
	}
	return ""
}

func eventInstance(event Event) (domain.Instance, bool) {
	raw, ok := event.Data["instance"]
	if !ok {
		return domain.Instance{}, false
	}
	if instance, ok := raw.(domain.Instance); ok {
		return instance, true
	}
	if values, ok := raw.(map[string]any); ok {
		return domain.Instance{ID: eventString(values, "id"), NodeID: eventString(values, "node_id")}, true
	}
	return domain.Instance{}, false
}

func eventJob(event Event) (domain.Job, bool) {
	raw, ok := event.Data["job"]
	if !ok {
		return domain.Job{}, false
	}
	if job, ok := raw.(domain.Job); ok {
		return job, true
	}
	if values, ok := raw.(map[string]any); ok {
		return domain.Job{ID: eventString(values, "id"), NodeID: eventString(values, "node_id"),
			InstanceID: eventString(values, "instance_id")}, true
	}
	return domain.Job{}, false
}

func eventString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func (s *Server) tokenAllowsAuditResource(ctx context.Context, principal Principal, resourceID string) bool {
	if !principal.HasResourceRestriction() {
		return true
	}
	if _, err := s.store.GetNode(ctx, resourceID); err == nil {
		return principal.AllowsNodePermission(resourceID, PermissionRead)
	}
	if instance, err := s.store.GetInstance(ctx, resourceID); err == nil {
		return principal.AllowsInstancePermission(instance, PermissionRead)
	}
	if job, err := s.store.GetJob(ctx, resourceID); err == nil {
		return principal.AllowsJobPermission(job, PermissionRead)
	}
	if _, err := uuid.Parse(resourceID); err != nil {
		return false
	}
	var instanceID, nodeID string
	err := s.store.pool.QueryRow(ctx, `SELECT instance_id::text,node_id::text FROM (
		SELECT s.instance_id,i.node_id FROM schedules s JOIN instances i ON i.id=s.instance_id WHERE s.id=$1
		UNION ALL SELECT p.instance_id,i.node_id FROM addon_packages p JOIN instances i ON i.id=p.instance_id WHERE p.id=$1
		UNION ALL SELECT b.instance_id,i.node_id FROM backups b JOIN instances i ON i.id=b.instance_id WHERE b.id=$1
	) scoped LIMIT 1`, resourceID).Scan(&instanceID, &nodeID)
	if err == nil {
		return principal.AllowsInstancePermission(domain.Instance{ID: instanceID, NodeID: nodeID}, PermissionRead)
	}
	return false
}

func (s *Server) auditEventAllowed(ctx context.Context, principal Principal, event domain.AuditEvent) bool {
	if !principal.HasResourceRestriction() {
		return true
	}
	switch event.ResourceType {
	case "node":
		return principal.AllowsNodePermission(event.ResourceID, PermissionRead)
	case "instance":
		if instance, err := s.store.GetInstance(ctx, event.ResourceID); err == nil {
			return principal.AllowsInstancePermission(instance, PermissionRead)
		}
	case "job":
		if job, err := s.store.GetJob(ctx, event.ResourceID); err == nil {
			return principal.AllowsJobPermission(job, PermissionRead)
		}
	}
	if instanceID := stringFromAny(event.Metadata["instance_id"]); instanceID != "" {
		if instance, err := s.store.GetInstance(ctx, instanceID); err == nil {
			return principal.AllowsInstancePermission(instance, PermissionRead)
		}
	}
	return s.tokenAllowsAuditResource(ctx, principal, event.ResourceID)
}

// enforceTokenResourceScope is a deny-by-default guard for resource-scoped principals.
// List endpoints additionally constrain their database queries in the handler.
func (s *Server) enforceTokenResourceScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		if !principal.HasResourceRestriction() {
			next.ServeHTTP(w, r)
			return
		}

		if nodeID := chi.URLParam(r, "nodeID"); nodeID != "" && !principal.AllowsNode(nodeID) {
			writeTokenScopeDenied(w)
			return
		}
		if instanceID := chi.URLParam(r, "instanceID"); instanceID != "" {
			instance, err := s.store.GetInstance(r.Context(), instanceID)
			if err != nil {
				s.fail(w, err)
				return
			}
			if !principal.AllowsInstance(instance) {
				writeTokenScopeDenied(w)
				return
			}
		}
		if jobID := chi.URLParam(r, "jobID"); jobID != "" {
			job, err := s.store.GetJob(r.Context(), jobID)
			if err != nil {
				s.fail(w, err)
				return
			}
			if !principal.AllowsJob(job) {
				writeTokenScopeDenied(w)
				return
			}
		}

		path := strings.TrimSuffix(r.URL.Path, "/")
		if path == "/api/v1/nodes/enrollment-tokens" || path == "/api/v1/users" ||
			strings.HasPrefix(path, "/api/v1/notifications") ||
			strings.HasPrefix(path, "/api/v1/notification-channels") ||
			strings.HasPrefix(path, "/api/v1/notification-deliveries") {
			writeTokenScopeDenied(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeTokenScopeDenied(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "resource_scope_denied", "resource is outside the authenticated principal's scope")
}
