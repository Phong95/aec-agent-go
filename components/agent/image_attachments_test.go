package agent_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/Phong95/aec-agent-go/components/agent"
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
