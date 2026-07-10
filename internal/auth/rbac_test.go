package auth

import (
	"testing"

	"github.com/google/uuid"
)

// The authorisation model, stated as a table. When someone changes who may do
// what, this table is what they must change too — and a reviewer can read the
// intended policy without reading the map.
func TestRolePermissions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		role       string
		permission string
		want       bool
	}{
		{RoleOwner, PermTenantManage, true},
		{RoleOwner, PermUserManage, true},
		{RoleOwner, PermCoursePublish, true},

		{RoleAdmin, PermUserManage, true},
		{RoleAdmin, PermCoursePublish, true},
		// Only an owner may reconfigure the workspace itself.
		{RoleAdmin, PermTenantManage, false},

		{RoleInstructor, PermCourseWrite, true},
		{RoleInstructor, PermCoursePublish, true},
		{RoleInstructor, PermUserRead, true},
		{RoleInstructor, PermUserManage, false},
		{RoleInstructor, PermTenantManage, false},

		{RoleStudent, PermCourseRead, true},
		{RoleStudent, PermCourseWrite, false},
		{RoleStudent, PermCoursePublish, false},
		{RoleStudent, PermUserRead, false},
		{RoleStudent, PermUserManage, false},
		{RoleStudent, PermTenantManage, false},

		// Marking is its own permission. Every teaching role has it; a student
		// grading their own essay would be a novel approach to assessment.
		{RoleOwner, PermSubmissionGrade, true},
		{RoleAdmin, PermSubmissionGrade, true},
		{RoleInstructor, PermSubmissionGrade, true},
		{RoleStudent, PermSubmissionGrade, false},
	}

	for _, tt := range tests {
		t.Run(tt.role+"/"+tt.permission, func(t *testing.T) {
			if got := Can(tt.role, tt.permission); got != tt.want {
				t.Errorf("Can(%q, %q) = %v, want %v", tt.role, tt.permission, got, tt.want)
			}
		})
	}
}

// An unknown role must grant nothing. A map lookup that misses returns the zero
// value, which is exactly the behaviour we want — but only if nobody "helpfully"
// adds a default case later.
func TestUnknownRoleGrantsNothing(t *testing.T) {
	t.Parallel()

	for _, permission := range []string{PermCourseRead, PermCourseWrite, PermTenantManage} {
		if Can("superuser", permission) {
			t.Errorf("an unknown role was granted %q", permission)
		}
		if Can("", permission) {
			t.Errorf("the empty role was granted %q", permission)
		}
	}
}

// An unknown permission must be denied even for an owner, so that a typo in a
// call site fails closed rather than open.
func TestUnknownPermissionIsDenied(t *testing.T) {
	t.Parallel()

	if Can(RoleOwner, "course:delete-everything") {
		t.Error("an owner was granted a permission that does not exist; typos must fail closed")
	}
}

func TestPrincipalCanDelegatesToRole(t *testing.T) {
	t.Parallel()

	student := Principal{UserID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), Role: RoleStudent}
	if !student.Can(PermCourseRead) {
		t.Error("a student cannot read courses")
	}
	if student.Can(PermCourseWrite) {
		t.Error("a student can write courses")
	}
}

func TestAuthorizeReturnsForbidden(t *testing.T) {
	t.Parallel()

	svc := &Service{}
	student := Principal{Role: RoleStudent}

	if err := svc.Authorize(student, PermCourseRead); err != nil {
		t.Errorf("a student was refused course:read: %v", err)
	}
	if err := svc.Authorize(student, PermCourseWrite); err == nil {
		t.Error("a student was authorised for course:write")
	}
}
