package grouphub

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Phong95/aec-agent-go/components/grouphub/group"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// A-108: who answers a user message in a group. A pure function, shared by the SDK's
// orchestrator and any UI that wants to show "who will answer" before sending.
//
// Order: the composer's To list, then @Name mentions in the text, then the group's
// reply mode. Muted members answer only when they are addressed directly.
// ---------------------------------------------------------------------------

// noMentionReason is the note of a "only when mentioned" message nobody was addressed in.
const noMentionReason = "Nobody was mentioned, so nobody answers. @mention a member or pick one in “To”."

// MentionedMembers returns the members text mentions as "@Name" (case-insensitive, the
// name followed by the end, a space or punctuation) or "@id", in group order. Longer
// names are matched first, so "@Spec Checker" is not also read as "@Spec".
func MentionedMembers(text string, members []MemberRef) []string {
	lower := jsstr.ToLower(text)
	found := map[string]bool{}
	// Spans of the text already claimed by a longer name.
	var claimed [][2]int
	type candidate struct{ id, token string }
	var candidates []candidate
	for _, m := range members {
		for _, token := range []string{jsstr.ToLower(jsstr.Trim(m.Name)), jsstr.ToLower(jsstr.Trim(m.ID))} {
			if token != "" {
				candidates = append(candidates, candidate{id: m.ID, token: token})
			}
		}
	}
	// Longest first (in UTF-16 units, like a JS string length); a stable sort keeps the
	// member order for equal lengths, like Array.prototype.sort.
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		return cmp.Compare(kernel.UTF16Len(b.token), kernel.UTF16Len(a.token))
	})
	for _, c := range candidates {
		needle := "@" + c.token
		for from := 0; from <= len(lower); {
			i := strings.Index(lower[from:], needle)
			if i < 0 {
				break
			}
			at := from + i
			end := at + len(needle)
			free := !slices.ContainsFunc(claimed, func(s [2]int) bool { return at < s[1] && end > s[0] })
			if boundaryAt(lower, end) && free {
				found[c.id] = true
				claimed = append(claimed, [2]int{at, end})
			}
			from = at + 1
		}
	}
	out := []string{}
	for _, m := range members {
		if found[m.ID] && !slices.Contains(out, m.ID) {
			out = append(out, m.ID)
		}
	}
	return out
}

// ResolveRecipients returns who was addressed and what the message sets off.
func ResolveRecipients(in RecipientInput) Recipients {
	isMember := map[string]bool{}
	memberIDs := make([]string, 0, len(in.Members))
	for _, m := range in.Members {
		isMember[m.ID] = true
		memberIDs = append(memberIDs, m.ID)
	}
	addressed := []string{}
	add := func(id string) {
		if isMember[id] && !slices.Contains(addressed, id) {
			addressed = append(addressed, id)
		}
	}
	for _, id := range in.To {
		add(id)
	}
	for _, id := range MentionedMembers(in.Text, in.Members) {
		add(id)
	}

	if len(addressed) == 1 {
		return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanSingle, AgentID: addressed[0]}}
	}
	if len(addressed) > 1 {
		return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanRound, AgentIDs: slices.Clone(addressed), Summarize: true}}
	}

	active := []string{}
	for _, id := range memberIDs {
		if !slices.Contains(in.Muted, id) || id == in.LeadID {
			active = append(active, id)
		}
	}
	switch in.ReplyMode {
	case group.ReplyMention:
		return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanNone, Reason: noMentionReason}}
	case group.ReplyAll:
		if len(active) == 1 {
			return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanSingle, AgentID: active[0]}}
		}
		return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanRound, AgentIDs: active, Summarize: len(active) > 1}}
	default: // group.ReplyLead
		helpers := []string{}
		for _, id := range active {
			if id != in.LeadID {
				helpers = append(helpers, id)
			}
		}
		if len(helpers) == 0 {
			return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanSingle, AgentID: in.LeadID}}
		}
		return Recipients{Addressed: addressed, Plan: group.Plan{Kind: group.PlanLead, LeadID: in.LeadID, Helpers: helpers}}
	}
}

// boundaryAt reports whether a mention ending at byte end of s ends there: the end of
// the text, or a character that is not a letter, a number, "_" or "-". A character
// outside the BMP is a boundary, as in core/ts (which tests one UTF-16 unit, a lone
// surrogate, against the name pattern).
func boundaryAt(s string, end int) bool {
	if end >= len(s) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s[end:])
	return !isNameChar(r)
}

// isNameChar is the /[\p{L}\p{N}_-]/u test of core/ts on one UTF-16 unit.
func isNameChar(r rune) bool {
	if r > 0xFFFF {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' || r == '-'
}
