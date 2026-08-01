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
