package org

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Typed errors
var (
	ErrOrgNotFound  = errors.New("organization not found")
	ErrOrgNameTaken = errors.New("organization name is taken")
)

type Org struct {
	ID      int64 `xorm:"pk autoincr 'id'"`
	Version int
	Name    string

	Address1 string
	Address2 string
	City     string
	ZipCode  string
	State    string
	Country  string

	Created time.Time
	Updated time.Time
}
type OrgUser struct {
	ID      int64 `xorm:"pk autoincr 'id'"`
	OrgID   int64 `xorm:"org_id"`
	UserID  int64 `xorm:"user_id"`
	Role    RoleType
	Created time.Time
	Updated time.Time
}

// swagger:enum RoleType
type RoleType string

const (
	RoleViewer RoleType = "Viewer"
	RoleEditor RoleType = "Editor"
	RoleAdmin  RoleType = "Admin"
)

type CreateOrgCommand struct {
	Name string `json:"name" binding:"Required"`

	// initial admin user for account
	UserID int64 `json:"-" xorm:"user_id"`
}

type GetOrgIDForNewUserCommand struct {
	Email        string
	Login        string
	OrgID        int64
	OrgName      string
	SkipOrgSetup bool
}

type GetUserOrgListQuery struct {
	UserID int64 `xorm:"user_id"`
}

type UserOrgDTO struct {
	OrgID int64    `json:"orgId" xorm:"org_id"`
	Name  string   `json:"name"`
	Role  RoleType `json:"role"`
}

type UpdateOrgCommand struct {
	Name  string
	OrgId int64
}

type SearchOrgsQuery struct {
	Query string
	Name  string
	Limit int
	Page  int
	IDs   []int64 `xorm:"ids"`
}

type OrgDTO struct {
	ID   int64  `json:"id" xorm:"id"`
	Name string `json:"name"`
}

type GetOrgByIdQuery struct {
	ID int64
}

type GetOrgByNameQuery struct {
	Name string
}

type UpdateOrgAddressCommand struct {
	OrgID int64 `xorm:"org_id"`
	Address
}

type Address struct {
	Address1 string `json:"address1"`
	Address2 string `json:"address2"`
	City     string `json:"city"`
	ZipCode  string `json:"zipCode"`
	State    string `json:"state"`
	Country  string `json:"country"`
}

type DeleteOrgCommand struct {
	ID int64 `xorm:"id"`
}

func (r RoleType) IsValid() bool {
	return r == RoleViewer || r == RoleAdmin || r == RoleEditor
}

func (r RoleType) Includes(other RoleType) bool {
	if r == RoleAdmin {
		return true
	}

	if r == RoleEditor {
		return other != RoleAdmin
	}

	return r == other
}

func (r RoleType) Children() []RoleType {
	switch r {
	case RoleAdmin:
		return []RoleType{RoleEditor, RoleViewer}
	case RoleEditor:
		return []RoleType{RoleViewer}
	default:
		return nil
	}
}

func (r RoleType) Parents() []RoleType {
	switch r {
	case RoleEditor:
		return []RoleType{RoleAdmin}
	case RoleViewer:
		return []RoleType{RoleEditor, RoleAdmin}
	default:
		return nil
	}
}

func (r *RoleType) UnmarshalText(data []byte) error {
	// make sure "viewer" and "Viewer" are both correct
	str := strings.Title(string(data))

	*r = RoleType(str)
	if !r.IsValid() {
		if (*r) != "" {
			return fmt.Errorf("invalid role value: %s", *r)
		}

		*r = RoleViewer
	}

	return nil
}

type ByOrgName []*UserOrgDTO

// Len returns the length of an array of organisations.
func (o ByOrgName) Len() int {
	return len(o)
}

// Swap swaps two indices of an array of organizations.
func (o ByOrgName) Swap(i, j int) {
	o[i], o[j] = o[j], o[i]
}

// Less returns whether element i of an array of organizations is less than element j.
func (o ByOrgName) Less(i, j int) bool {
	if strings.ToLower(o[i].Name) < strings.ToLower(o[j].Name) {
		return true
	}

	return o[i].Name < o[j].Name
}
