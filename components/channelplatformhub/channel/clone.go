package channel

import (
	"maps"
	"slices"
)

// Clone returns a deep copy: Credentials, Access.AllowFrom, Access.Groups, Account and Home are copied, so a
// caller and a store never share a mutable part of a channel. Add a line here when a
// pointer, slice or map field is added to Channel; the clonecheck test fails until you do.
func (c Channel) Clone() Channel {
	if c.Credentials != nil {
		c.Credentials = maps.Clone(c.Credentials)
	}
	if c.Access.AllowFrom != nil {
		c.Access.AllowFrom = slices.Clone(c.Access.AllowFrom)
	}
	if c.Access.Groups != nil {
		c.Access.Groups = slices.Clone(c.Access.Groups)
	}
	if c.Account != nil {
		a := *c.Account
		c.Account = &a
	}
	if c.Home != nil {
		h := *c.Home
		c.Home = &h
	}
	return c
}
