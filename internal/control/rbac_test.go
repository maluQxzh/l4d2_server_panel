package control

import (
	"testing"

	"github.com/l4d2-panel/l4d2panel/internal/domain"
)

func TestRolePermissions(t *testing.T) {
	tests := []struct {
		role       domain.Role
		permission Permission
		want       bool
	}{
		{domain.RoleViewer, PermissionRead, true},
		{domain.RoleViewer, PermissionOperate, false},
		{domain.RoleOperator, PermissionOperate, true},
		{domain.RoleOperator, PermissionManage, false},
		{domain.RoleAdmin, PermissionManage, true},
		{domain.RoleAdmin, PermissionRootSecurity, false},
		{domain.RoleOwner, PermissionRootSecurity, true},
	}
	for _, test := range tests {
		if got := RoleAllows(test.role, test.permission); got != test.want {
			t.Errorf("RoleAllows(%q,%q)=%v, want %v", test.role, test.permission, got, test.want)
		}
	}
}

func TestExplicitBindingPermissionsOverrideRolePreset(t *testing.T) {
	binding := domain.RoleBinding{
		Role:        domain.RoleAdmin,
		ScopeType:   "instance",
		Permissions: []string{string(PermissionInstancesRead), string(PermissionBackupsCreate)},
	}
	if !bindingAllows(binding, PermissionBackupsCreate) {
		t.Fatal("explicit backup permission was not granted")
	}
	if bindingAllows(binding, PermissionRCONExecute) || bindingAllows(binding, PermissionManage) {
		t.Fatal("admin preset leaked permissions outside the explicit set")
	}
}

func TestGlobalAuditRequiresSeparateManagementPermission(t *testing.T) {
	viewer := domain.RoleBinding{Role: domain.RoleViewer, ScopeType: "global", Permissions: presetPermissions(domain.RoleViewer)}
	admin := domain.RoleBinding{Role: domain.RoleAdmin, ScopeType: "global", Permissions: presetPermissions(domain.RoleAdmin)}
	if !bindingAllows(viewer, PermissionAuditRead) || bindingAllows(viewer, PermissionAuditReadGlobal) {
		t.Fatal("viewer audit preset crossed the global audit boundary")
	}
	if !bindingAllows(admin, PermissionAuditReadGlobal) {
		t.Fatal("administrator preset did not include global audit")
	}
}
