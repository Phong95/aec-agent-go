package job

import (
	"slices"
	"strings"
)

// MatchHomes picks the home chats named: by channel name, platform or id, ignoring case.
// The second result is an error for the model, "" when every name matched. Shared by the
// job tool (deliver_channels) and the channel tool (A-219), so "telegram" means the same
// to both.
func MatchHomes(homes []HomeChat, names []string) ([]HomeChat, string) {
	var picked []HomeChat
	for _, name := range names {
		found := false
		for _, h := range homes {
			if h.Named(name) {
				found = true
				if !slices.Contains(picked, h) {
					picked = append(picked, h)
				}
			}
		}
		if !found {
			return nil, `no channel "` + name + "\" has a home chat — " + HomesText(homes)
		}
	}
	return picked, ""
}

// Named reports whether this is the channel called name (its name, platform or id,
// ignoring case).
func (h HomeChat) Named(name string) bool {
	return strings.EqualFold(h.Name, name) || strings.EqualFold(h.Platform, name) || h.ChannelID == name
}

// HomesText names the channels that have a home chat, for the model.
func HomesText(homes []HomeChat) string {
	if len(homes) == 0 {
		return "no channel has a home chat yet (the user sends /sethome in the chat that should get the results)"
	}
	names := make([]string, 0, len(homes))
	for _, h := range homes {
		names = append(names, `"`+h.Name+`" (`+h.Platform+`)`)
	}
	return "channels with a home chat: " + strings.Join(names, ", ")
}
