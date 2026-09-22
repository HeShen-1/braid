package auth

import (
	"errors"
	"reflect"
	"testing"
)

func TestAllRolesOrder(t *testing.T) {
	want := []Role{RoleAdmin, RoleAuthor, RoleApprover}
	if got := AllRoles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllRoles() = %v, want %v(声明序 = DDL ENUM 序)", got, want)
	}
	// 防御性拷贝:调用方改快照不得污染词表真源。
	got := AllRoles()
	got[0] = Role("root")
	if AllRoles()[0] != RoleAdmin {
		t.Fatalf("AllRoles 返回可变底层数组,词表真源被污染")
	}
}

func TestValidRoles(t *testing.T) {
	cases := []struct {
		name string
		in   []Role
		ok   bool
	}{
		{"single", []Role{RoleAdmin}, true},
		{"all three", []Role{RoleAdmin, RoleAuthor, RoleApprover}, true},
		{"empty", nil, false},
		{"unknown role", []Role{Role("root")}, false},
		{"mixed unknown", []Role{RoleAuthor, Role("owner")}, false},
		{"duplicate", []Role{RoleAdmin, RoleAdmin}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validRoles(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("validRoles(%v) = %v, want nil", tc.in, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("validRoles(%v) = nil, want error", tc.in)
				}
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("validRoles(%v) = %v, want ErrInvalidInput", tc.in, err)
				}
			}
		})
	}
}
