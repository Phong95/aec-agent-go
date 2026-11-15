package agent_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
	"github.com/Phong95/aec-agent-go/components/agent/profile"
	"github.com/Phong95/aec-agent-go/kernel"
)

// A-165: images attached to a turn reach the model with the user's message.

var dotPNG = kernel.MessageAttachment{
	Type:     kernel.AttachmentTypeImage,
	MIMEType: "image/png",
	Data:     "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==",
	Name:     "dot.png",
}

func TestChatSendsAttachedImages(t *testing.T) {
	e := newEnv(t)
	a := e.spawn(t, testProfile("vision"))
	s, err := a.StartSession(t.Context())
	mustOK(t, err)
	_, err = a.Chat(t.Context(), "What is this?", agent.WithChatAttachments(dotPNG))
	mustOK(t, err)

	calls := e.mocks["primary"].Calls()
	if len(calls) == 0 {
		t.Fatal("no model call")
	}
	i := slices.IndexFunc(calls[0].Messages, func(m kernel.Message) bool { return m.Role == kernel.RoleUser })
	if i < 0 || !slices.Equal(calls[0].Messages[i].Attachments, []kernel.MessageAttachment{dotPNG}) {
		t.Fatalf("user message sent = %+v", calls[0].Messages)
	}
	stored, err := e.hub.Messages.Get(t.Context(), s.ID)
	mustOK(t, err)
	if len(stored) == 0 || !slices.Equal(stored[0].Attachments, []kernel.MessageAttachment{dotPNG}) {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestChatRefusesInvalidAttachments(t *testing.T) {
	e := newEnv(t)
	a := e.spawn(t, testProfile("vision"))
	s, err := a.StartSession(t.Context())
	mustOK(t, err)
	svg := dotPNG
	svg.MIMEType = "image/svg+xml"
	_, err = a.Chat(t.Context(), "hi", agent.WithChatAttachments(svg))
	var me *kernel.ModelError
	if !errors.As(err, &me) || me.Code != kernel.ModelCodeInvalidRequest {
		t.Fatalf("err = %v, want an invalid_request ModelError", err)
	}
	if n := len(e.mocks["primary"].Calls()); n != 0 {
		t.Fatalf("%d model calls", n)
	}
	stored, err := e.hub.Messages.Get(t.Context(), s.ID)
	mustOK(t, err)
	if len(stored) != 0 {
		t.Fatalf("stored = %+v", stored)
	}
}

// A-222: a turn takes up to the agent's LoopConfig.MaxImagesPerMessage; without it a 5th
// image is still refused.
func TestChatTakesUpToMaxImagesPerMessage(t *testing.T) {
	images := func(n int) []kernel.MessageAttachment {
		out := make([]kernel.MessageAttachment, n)
		for i := range out {
			out[i] = dotPNG
		}
		return out
	}
	refused := func(err error) bool {
		var me *kernel.ModelError
		return errors.As(err, &me) && me.Code == kernel.ModelCodeInvalidRequest
	}

	e := newEnv(t)
	p := testProfile("vision-8")
	eight := 8
	p.LoopConfig = &profile.LoopConfig{MaxImagesPerMessage: &eight}
	a := e.spawn(t, p)
	_, err := a.StartSession(t.Context())
	mustOK(t, err)
	_, err = a.Chat(t.Context(), "Eight?", agent.WithChatAttachments(images(8)...))
	mustOK(t, err)
	calls := e.mocks["primary"].Calls()
	i := slices.IndexFunc(calls[0].Messages, func(m kernel.Message) bool { return m.Role == kernel.RoleUser })
	if i < 0 || len(calls[0].Messages[i].Attachments) != 8 {
		t.Fatalf("user message sent = %+v", calls[0].Messages)
	}
	if _, err = a.Chat(t.Context(), "Nine?", agent.WithChatAttachments(images(9)...)); !refused(err) || err.Error() != "at most 8 images per message" {
		t.Fatalf("9 images: err = %v, want an invalid_request ModelError", err)
	}

	d := e.spawn(t, testProfile("vision-default"))
	_, err = d.StartSession(t.Context())
	mustOK(t, err)
	if _, err = d.Chat(t.Context(), "Five?", agent.WithChatAttachments(images(kernel.MaxImagesPerMessage+1)...)); !refused(err) {
		t.Fatalf("5 images by default: err = %v, want an invalid_request ModelError", err)
	}
}
