package channel

import "encoding/json"

// DefaultAccess is the access of a channel that sets none: pairing in direct messages,
// groups ignored, and in groups only messages that mention the bot.
func DefaultAccess() Access {
	return Access{DmPolicy: DmPairing, AllowFrom: []string{}, GroupPolicy: GroupDisabled, Groups: []string{}, RequireMention: true}
}

// DefaultSettings are the settings of a channel that sets none: streamed replies and the
// tool-progress message.
func DefaultSettings() Settings {
	return Settings{Streaming: true, ToolProgress: true}
}

// WithDefaults returns the channel with the fields added after it was saved (phase 2)
// set to their defaults, and nil lists made empty. Stores may hand back older rows; the
// agent applies it on every read.
func (c Channel) WithDefaults() Channel {
	if c.Access.DmPolicy == "" {
		c.Access.DmPolicy = DmPairing
	}
	if c.Access.AllowFrom == nil {
		c.Access.AllowFrom = []string{}
	}
	if c.Access.GroupPolicy == "" {
		c.Access.GroupPolicy = GroupDisabled
	}
	if c.Access.Groups == nil {
		c.Access.Groups = []string{}
	}
	return c
}

// UnmarshalJSON reads an Access; a field that is absent (a row saved before phase 2)
// takes its DefaultAccess value, so a missing requireMention reads as true.
func (a *Access) UnmarshalJSON(b []byte) error {
	type plain Access
	v := plain(DefaultAccess())
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*a = Access(v)
	return nil
}

// UnmarshalJSON reads Settings; an absent field takes its DefaultSettings value.
func (s *Settings) UnmarshalJSON(b []byte) error {
	type plain Settings
	v := plain(DefaultSettings())
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = Settings(v)
	return nil
}

// UnmarshalJSON reads a Channel; an absent access or settings takes its default value.
func (c *Channel) UnmarshalJSON(b []byte) error {
	type plain Channel
	v := plain{Access: DefaultAccess(), Settings: DefaultSettings()}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*c = Channel(v)
	return nil
}
