package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Phong95/aec-agent-go/components/channelplatformhub/channel"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// promptTimeout is how long an approval or a question waits for an answer in the chat.
const promptTimeout = 5 * time.Minute

// Limits of a prompt's text, in UTF-16 units as core/ts counts.
const (
	maxPromptArgumentsChars   = 1200
	maxPromptDescriptionChars = 300
	maxPromptAnswerChars      = 4000
	maxPromptQuestionChars    = 2000
	maxPromptReasonChars      = 500
	maxPromptChoiceLabel      = 40
)

// promptButtonPrefix starts the data of a prompt's buttons: "p:<prompt id>:<choice>", e.g.
// "p:3:y", "p:4:c1".
const promptButtonPrefix = "p:"

// The symbols of the prompts and the turn view, built from rune values so this source
// stays ASCII-only.
var (
	symHourglass = string(rune(0x231b))
	symSpeech    = string(rune(0x1f4ac))
	symCheck     = string(rune(0x2705))
	symCross     = string(rune(0x274c))
	symWarning   = string([]rune{0x26a0, 0xfe0f})
	symRunning   = string(rune(0x23f3))
	symEllipsis  = string(rune(0x2026))
)

// promptChat is the chat a prompt waits in, and who may answer it.
type promptChat struct {
	channelID string
	// key is the chat's key (channel id + chat key): the manager's per-chat maps use it.
	key       string
	target    channel.Target
	chatType  channel.ChatType
	initiator channel.Sender
}

// channelPromptsDeps is what channelPrompts needs.
type channelPromptsDeps struct {
	// send sends a prompt to its chat; false when it could not be sent.
	send func(ctx context.Context, chat promptChat, m channel.OutboundMessage) (channel.MessageRef, bool)
	// edit edits a sent prompt (settled: text updated, buttons removed). Best effort.
	edit func(chat promptChat, ref channel.MessageRef, m channel.OutboundMessage)
	// async runs f in a goroutine the manager waits for on close.
	async func(f func())
	// buttons is the channel's buttons capability.
	buttons func(channelID string) bool
	// approvers are the sender ids allowed to approve tools in group chats
	// (Channel.Access.AllowFrom).
	approvers func(channelID string) []string
	agentName func() string
	timeout   time.Duration
}

// promptOutcome is how a prompt was settled; err is set when nobody answered.
type promptOutcome struct {
	approved bool
	answer   string
	reason   string
	by       string
	err      error
}

// failure is why nobody answered ("" when someone did).
func (o promptOutcome) failure() string {
	if o.err == nil {
		return ""
	}
	return o.err.Error()
}

type prompt struct {
	id   string
	kind string // "approval" or "clarify"
	chat promptChat
	// choices are the clarify choices, answerable by number or button.
	choices []string
	// text is the prompt's text, kept to show how it was settled.
	text    string
	ref     *channel.MessageRef
	timer   *time.Timer
	done    chan promptOutcome
	outcome *promptOutcome
}

// channelPrompts are the approvals and clarify questions asked in the chat a channel turn
// came from (A-100 phase 2), the ChannelPrompts of core/ts: the channel side of the
// interaction router. The agent loop asks the agent for the session's
// InteractionHandler, and channel sessions get one that ends up here.
//
//   - Approval: the tool, its arguments and why it needs approval, with Approve / Deny
//     buttons when the platform has them, else /approve / /deny. In a direct message the
//     sender approves. In a group only senders in Access.AllowFrom may approve; with
//     nobody allowed it is denied at once.
//   - Clarify: the question and its choices (buttons, or answer by number). The next
//     message of the sender who started the turn is the answer.
//   - No answer within the timeout: an approval is denied, a question fails, the same
//     fail-closed rules as the web UI. The turn ending cancels both.
//
// It is safe for concurrent use.
type channelPrompts struct {
	d channelPromptsDeps

	mu sync.Mutex
	// prompts are the pending prompts, oldest first.
	prompts []*prompt
	seq     int
}

func newChannelPrompts(d channelPromptsDeps) *channelPrompts {
	if d.timeout <= 0 {
		d.timeout = promptTimeout
	}
	if d.async == nil {
		d.async = func(f func()) { go f() }
	}
	return &channelPrompts{d: d}
}

// requestApproval asks the chat whether a tool may run.
func (p *channelPrompts) requestApproval(ctx context.Context, chat promptChat, req toolhub.ApprovalRequest) (toolhub.ApprovalDecision, error) {
	if chat.chatType != channel.ChatDM && len(p.d.approvers(chat.channelID)) == 0 {
		reason := "nobody in this group chat is allowed to approve tools"
		return toolhub.ApprovalDecision{Approved: false, Reason: &reason}, nil
	}
	lines := []string{
		p.d.agentName() + " wants to run " + req.ToolName + ".",
		truncate16(firstParagraph(req.ToolDescription), maxPromptDescriptionChars),
	}
	if req.Reason != "" {
		lines = append(lines, "Why approval is needed: "+req.Reason)
	}
	if req.Sandbox != nil {
		where := "not isolated from the host"
		if req.Sandbox.Isolated {
			where = "isolated"
		}
		lines = append(lines, "Where: "+req.Sandbox.ID+" ("+where+")")
	}
	if args := formatPromptArguments(req.Arguments); args != "" {
		lines = append(lines, "", args)
	}
	out := p.open(ctx, chat, "approval", "Approval needed", lines, nil)
	if failure := out.failure(); failure != "" {
		// A timeout or a cancelled turn is a denial, not an error: the model sees "user declined".
		return toolhub.ApprovalDecision{Approved: false, Reason: &failure}, nil
	}
	decision := toolhub.ApprovalDecision{Approved: out.approved}
	if out.reason != "" {
		reason := out.reason
		decision.Reason = &reason
	}
	return decision, nil
}

// askUser asks the chat a clarify question.
func (p *channelPrompts) askUser(ctx context.Context, chat promptChat, req toolhub.ClarifyRequest) (toolhub.ClarifyResponse, error) {
	var choices []string
	for _, c := range req.Choices {
		if jsstr.Trim(c) != "" {
			choices = append(choices, c)
		}
	}
	lines := []string{truncate16(jsstr.Trim(req.Question), maxPromptQuestionChars)}
	if len(choices) > 0 {
		lines = append(lines, "")
		for i, c := range choices {
			lines = append(lines, strconv.Itoa(i+1)+". "+c)
		}
	}
	out := p.open(ctx, chat, "clarify", "Question", lines, choices)
	if out.err != nil {
		return toolhub.ClarifyResponse{}, out.err
	}
	return toolhub.ClarifyResponse{Answer: out.answer}, nil
}

// answerText is a message in a chat with a pending question: the answer when it comes
// from the sender who started the turn. Commands are not answers.
func (p *channelPrompts) answerText(key string, sender channel.Sender, text string) bool {
	p.mu.Lock()
	pr := p.oldestLocked(key, "clarify")
	p.mu.Unlock()
	if pr == nil || sender.ID != pr.chat.initiator.ID {
		return false
	}
	trimmed := jsstr.Trim(text)
	answer := trimmed
	if n, err := strconv.Atoi(trimmed); err == nil && isDigits(trimmed) && n >= 1 && n <= len(pr.choices) {
		answer = pr.choices[n-1]
	}
	p.finish(pr.id, promptOutcome{answer: truncateUnits(answer, maxPromptAnswerChars), by: senderLabel(sender)})
	return true
}

// answerCommand is /approve [id] or /deny [id] [reason] in a chat. It returns a notice
// to reply with ("" for none).
func (p *channelPrompts) answerCommand(key string, sender channel.Sender, approved bool, args string) string {
	fields := jsstr.Fields(args)
	first := ""
	if len(fields) > 0 {
		first = fields[0]
	}
	p.mu.Lock()
	var byID *prompt
	if first != "" {
		byID = p.findLocked(strings.TrimPrefix(first, "#"))
	}
	pr := byID
	if pr == nil || pr.chat.key != key || pr.kind != "approval" {
		pr = p.oldestLocked(key, "approval")
	}
	p.mu.Unlock()
	if pr == nil {
		return "Nothing is waiting for approval."
	}
	if !p.mayApprove(pr, sender) {
		return "You are not allowed to approve tools here."
	}
	rest := fields
	if byID == pr {
		rest = fields[1:]
	}
	reason := truncateUnits(jsstr.Trim(strings.Join(rest, " ")), maxPromptReasonChars)
	p.finish(pr.id, promptOutcome{approved: approved, reason: reason, by: senderLabel(sender)})
	return ""
}

// answerButton is a button press on a prompt. It reports whether the press was a
// prompt's, and a notice to reply with ("" for none). Unknown or settled prompts are
// ignored.
func (p *channelPrompts) answerButton(key string, sender channel.Sender, data string) (bool, string) {
	if !strings.HasPrefix(data, promptButtonPrefix) {
		return false, ""
	}
	id, choice, _ := strings.Cut(strings.TrimPrefix(data, promptButtonPrefix), ":")
	p.mu.Lock()
	pr := p.findLocked(id)
	p.mu.Unlock()
	if pr == nil || pr.chat.key != key {
		return true, ""
	}
	if pr.kind == "approval" {
		if !p.mayApprove(pr, sender) {
			return true, "You are not allowed to approve tools here."
		}
		p.finish(pr.id, promptOutcome{approved: choice == "y", by: senderLabel(sender)})
		return true, ""
	}
	if sender.ID != pr.chat.initiator.ID {
		return true, ""
	}
	index, err := strconv.Atoi(strings.TrimPrefix(choice, "c"))
	if err == nil && index >= 0 && index < len(pr.choices) {
		p.finish(pr.id, promptOutcome{answer: pr.choices[index], by: senderLabel(sender)})
	}
	return true, ""
}

// hasQuestion reports whether a chat has a question waiting for an answer.
func (p *channelPrompts) hasQuestion(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.oldestLocked(key, "clarify") != nil
}

// cancelChat cancels a chat's prompts (its turn ended or was stopped).
func (p *channelPrompts) cancelChat(key, reason string) {
	p.cancelWhere(func(pr *prompt) bool { return pr.chat.key == key }, reason)
}

// cancelPrefix cancels every prompt of the chats whose key starts with prefix (a
// channel stopped).
func (p *channelPrompts) cancelPrefix(prefix, reason string) {
	p.cancelWhere(func(pr *prompt) bool { return strings.HasPrefix(pr.chat.key, prefix) }, reason)
}

func (p *channelPrompts) cancelWhere(match func(*prompt) bool, reason string) {
	p.mu.Lock()
	var ids []string
	for _, pr := range p.prompts {
		if match(pr) {
			ids = append(ids, pr.id)
		}
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.finish(id, promptOutcome{err: errors.New(reason)})
	}
}

// ---------------------------------------------------------------------------

// open sends a prompt and waits for its outcome, the timeout, or ctx (the run stopped).
func (p *channelPrompts) open(ctx context.Context, chat promptChat, kind, title string, lines, choices []string) promptOutcome {
	p.mu.Lock()
	p.seq++
	id := strconv.Itoa(p.seq)
	p.mu.Unlock()
	buttons := p.d.buttons(chat.channelID)
	var hint string
	switch {
	case kind == "approval" && buttons:
		hint = "Or reply /approve " + id + " or /deny " + id + " <reason>."
	case kind == "approval":
		hint = "Reply /approve " + id + " or /deny " + id + " <reason>."
	case len(choices) > 0:
		hint = "Reply with your answer or its number."
	default:
		hint = "Reply with your answer."
	}
	all := append(append([]string{title + " (#" + id + ")"}, lines...), "", hint)
	text := jsstr.Trim(strings.Join(all, "\n"))

	pr := &prompt{id: id, kind: kind, chat: chat, choices: choices, text: text, done: make(chan promptOutcome, 1)}
	p.mu.Lock()
	p.prompts = append(p.prompts, pr)
	pr.timer = time.AfterFunc(p.d.timeout, func() {
		minutes := int(p.d.timeout.Round(time.Minute) / time.Minute)
		if minutes < 1 {
			minutes = 1
		}
		p.finish(id, promptOutcome{err: fmt.Errorf("nobody answered in the chat within %d min", minutes)})
	})
	p.mu.Unlock()

	m := channel.OutboundMessage{Text: text}
	if buttons {
		m.Buttons = promptButtons(id, kind, choices)
	}
	ref, ok := p.d.send(ctx, chat, m)
	if !ok {
		p.finish(id, promptOutcome{err: errors.New("the question could not be sent to the chat")})
	} else {
		p.mu.Lock()
		settled := pr.outcome != nil
		if !settled {
			pr.ref = &ref
		}
		p.mu.Unlock()
		if settled {
			p.d.async(func() { p.d.edit(chat, ref, channel.OutboundMessage{Text: settledText(pr)}) })
		}
	}

	select {
	case out := <-pr.done:
		return out
	case <-ctx.Done():
		p.finish(id, promptOutcome{err: errors.New("the run was stopped")})
		return <-pr.done
	}
}

// finish settles a pending prompt once; later calls do nothing.
func (p *channelPrompts) finish(id string, out promptOutcome) {
	p.mu.Lock()
	i := slices.IndexFunc(p.prompts, func(pr *prompt) bool { return pr.id == id })
	if i < 0 {
		p.mu.Unlock()
		return
	}
	pr := p.prompts[i]
	p.prompts = slices.Delete(p.prompts, i, i+1)
	if pr.timer != nil {
		pr.timer.Stop()
	}
	pr.outcome = &out
	ref := pr.ref
	p.mu.Unlock()
	if ref != nil {
		text := settledText(pr)
		r := *ref
		p.d.async(func() { p.d.edit(pr.chat, r, channel.OutboundMessage{Text: text}) })
	}
	pr.done <- out
}

func (p *channelPrompts) findLocked(id string) *prompt {
	for _, pr := range p.prompts {
		if pr.id == id {
			return pr
		}
	}
	return nil
}

func (p *channelPrompts) oldestLocked(key, kind string) *prompt {
	for _, pr := range p.prompts {
		if pr.chat.key == key && pr.kind == kind {
			return pr
		}
	}
	return nil
}

func (p *channelPrompts) mayApprove(pr *prompt, sender channel.Sender) bool {
	if slices.Contains(p.d.approvers(pr.chat.channelID), sender.ID) {
		return true
	}
	return pr.chat.chatType == channel.ChatDM && sender.ID == pr.chat.initiator.ID
}

func promptButtons(id, kind string, choices []string) [][]channel.Button {
	if kind == "approval" {
		return [][]channel.Button{{
			{Label: symCheck + " Approve", Data: promptButtonPrefix + id + ":y"},
			{Label: symCross + " Deny", Data: promptButtonPrefix + id + ":n"},
		}}
	}
	rows := make([][]channel.Button, 0, len(choices))
	for i, c := range choices {
		rows = append(rows, []channel.Button{{Label: truncate16(c, maxPromptChoiceLabel), Data: promptButtonPrefix + id + ":c" + strconv.Itoa(i)}})
	}
	return rows
}

// settledText is the prompt's text with how it was settled, and without the reply hint.
// The prompt is settled (its outcome is set) before this is called.
func settledText(pr *prompt) string {
	lines := strings.Split(pr.text, "\n")
	body := jsstr.Trim(strings.Join(lines[:len(lines)-1], "\n"))
	out := pr.outcome
	var line string
	switch {
	case out == nil:
		line = symHourglass + " cancelled"
	case out.err != nil:
		line = symHourglass + " " + out.err.Error()
	case pr.kind == "clarify":
		by := out.by
		if by == "" {
			by = "Answered"
		}
		line = symSpeech + " " + by + ": " + truncate16(out.answer, 200)
	default:
		line = symCross + " Denied"
		if out.approved {
			line = symCheck + " Approved"
		}
		if out.by != "" {
			line += " by " + out.by
		}
		if out.reason != "" {
			line += ": " + out.reason
		}
	}
	return body + "\n\n" + line
}

// formatPromptArguments pretty-prints the tool arguments (2-space indent, as
// JSON.stringify(args, null, 2)); "" for none.
func formatPromptArguments(args []byte) string {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
		return ""
	}
	return truncate16(buf.String(), maxPromptArgumentsChars)
}

func firstParagraph(text string) string {
	t := jsstr.Trim(text)
	lines := strings.Split(t, "\n")
	var out []string
	for _, line := range lines {
		if jsstr.Trim(line) == "" {
			break
		}
		out = append(out, line)
	}
	return jsstr.Trim(strings.Join(out, "\n"))
}

func senderLabel(s channel.Sender) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Username != "":
		return "@" + s.Username
	default:
		return s.ID
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// units16 is the length of s in UTF-16 units, as JS counts.
func units16(s string) int { return len(utf16.Encode([]rune(s))) }

// truncateUnits keeps the first maxLen UTF-16 units of s (s.slice(0, max) in JS).
func truncateUnits(s string, maxLen int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= maxLen {
		return s
	}
	return string(utf16.Decode(u[:maxLen]))
}

// truncate16 shortens s to maxLen UTF-16 units, ending it with an ellipsis when cut.
func truncate16(s string, maxLen int) string {
	if units16(s) <= maxLen {
		return s
	}
	return truncateUnits(s, maxLen-1) + symEllipsis
}
