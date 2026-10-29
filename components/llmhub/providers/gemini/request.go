package gemini

import (
	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/components/toolhub"
	"github.com/Phong95/aec-agent-go/internal/jsonx"
	"github.com/Phong95/aec-agent-go/kernel"
)

// ---------------------------------------------------------------------------
// Google Gemini API (generativelanguage.googleapis.com) request shapes
// ---------------------------------------------------------------------------
// Shapes below are intentionally minimal (only what this adapter writes), with the
// keys in a fixed order.

// part is one part of a content: a text, a function call (with its thought
// signature) or a function response.
type part struct {
	Text             *string           `json:"text,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	// InlineData (A-165) is an attached image, base64.
	InlineData *inlineData `json:"inlineData,omitempty"`
}

// inlineData (A-165) is {mimeType, data}.
type inlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

// functionCall is {name, args}. The call id is not sent: Gemini pairs a response
// with its call by name and order, and the SDK id may be one this provider made up.
type functionCall struct {
	Name string           `json:"name"`
	Args kernel.Arguments `json:"args"`
}

// functionResponse is {name, response}: response is {output} for a result and
// {error} for a failed call.
type functionResponse struct {
	Name     string         `json:"name"`
	Response responseObject `json:"response"`
}

// responseObject is the response object of a function response; exactly one member
// is set.
type responseObject struct {
	Output *string `json:"output,omitempty"`
	Error  *string `json:"error,omitempty"`
}

// content is {role, parts}: role is "user" or "model".
type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

// systemInstruction is {parts}.
type systemInstruction struct {
	Parts []part `json:"parts"`
}

// functionDeclaration is one tool. parametersJsonSchema takes full JSON Schema;
// the older parameters field only an OpenAPI subset.
type functionDeclaration struct {
	Name                 string             `json:"name"`
	Description          string             `json:"description,omitempty"`
	ParametersJSONSchema toolhub.JSONSchema `json:"parametersJsonSchema"`
}

// tool is {functionDeclarations}.
type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

// generationConfig holds the inference settings that were set.
type generationConfig struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	// ThinkingConfig (A-164) carries ModelOptions.ReasoningEffort as a thinking level.
	ThinkingConfig *thinkingConfig `json:"thinkingConfig,omitempty"`
}

// thinkingConfig (A-164) is {thinkingLevel}: Gemini 3 models take thinking levels, and
// the API rejects a level the model does not accept.
type thinkingConfig struct {
	ThinkingLevel string `json:"thinkingLevel"`
}

// requestBody is the JSON the provider sends, with the keys in a fixed order
// (contents, systemInstruction, tools, generationConfig); every key but contents is
// absent when it has nothing.
type requestBody struct {
	// Contents is always present, an empty list encodes [].
	Contents          []content          `json:"contents"`
	SystemInstruction *systemInstruction `json:"systemInstruction,omitempty"`
	Tools             []tool             `json:"tools,omitempty"`
	GenerationConfig  *generationConfig  `json:"generationConfig,omitempty"`
}

// textPart is a part holding text.
func textPart(s string) part { return part{Text: &s} }

// contentsBuilder collects contents and merges parts of the same role into one
// content: Gemini wants user and model turns to alternate.
type contentsBuilder struct {
	contents []content
}

func (b *contentsBuilder) push(role string, parts []part) {
	if len(parts) == 0 {
		return
	}
	if n := len(b.contents); n > 0 && b.contents[n-1].Role == role {
		b.contents[n-1].Parts = append(b.contents[n-1].Parts, parts...)
		return
	}
	b.contents = append(b.contents, content{Role: role, Parts: parts})
}

// mapContents maps the conversation onto Gemini contents and the system parts:
//   - ModelOptions.SystemPrompt comes first in the system instruction, then every
//     non-empty system message in order;
//   - an assistant message becomes a model content with its text (when not empty)
//     and its function calls; the first call carries its signature, or
//     SkipSignature when it has none, a later call only its own signature; then its
//     tool results follow as a user content;
//   - on any other message the tool results become function responses of the user
//     role, named after the call they answer (the call id when no call has it), and
//     a user message's non-empty text follows them;
//   - parts of the same role in a row are merged into one content.
func mapContents(msgs []kernel.Message, opts llmhub.ModelOptions) ([]content, []part) {
	var system []part
	if opts.SystemPrompt != "" {
		system = append(system, textPart(opts.SystemPrompt))
	}
	// A function response must name its function; tool results only carry the call id.
	names := map[string]string{}
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			names[tc.ID] = tc.Name
		}
	}
	responses := func(results []kernel.ToolResult) []part {
		parts := make([]part, 0, len(results))
		for _, tr := range results {
			name, ok := names[tr.ToolCallID]
			if !ok {
				name = tr.ToolCallID
			}
			text := tr.Content
			resp := responseObject{Output: &text}
			if tr.IsError != nil && *tr.IsError {
				resp = responseObject{Error: &text}
			}
			parts = append(parts, part{FunctionResponse: &functionResponse{Name: name, Response: resp}})
		}
		return parts
	}

	var b contentsBuilder
	for _, msg := range msgs {
		switch msg.Role {
		case kernel.RoleSystem:
			if msg.Content != "" {
				system = append(system, textPart(msg.Content))
			}
		case kernel.RoleAssistant:
			var parts []part
			if msg.Content != "" {
				parts = append(parts, textPart(msg.Content))
			}
			for i, tc := range msg.ToolCalls {
				p := part{FunctionCall: &functionCall{Name: tc.Name, Args: tc.Arguments}, ThoughtSignature: tc.Signature}
				// Only the first call of a step needs a signature; parallel calls after it have none.
				if p.ThoughtSignature == "" && i == 0 {
					p.ThoughtSignature = SkipSignature
				}
				parts = append(parts, p)
			}
			b.push("model", parts)
			b.push("user", responses(msg.ToolResults))
		default:
			// Tool results come from the user role, before any text the message also has.
			b.push("user", responses(msg.ToolResults))
			if msg.Role != kernel.RoleTool {
				// A-165: attached images before the text.
				var parts []part
				for _, img := range msg.Images() {
					parts = append(parts, part{InlineData: &inlineData{MIMEType: img.MIMEType, Data: img.Data}})
				}
				if msg.Content != "" {
					parts = append(parts, textPart(msg.Content))
				}
				if len(parts) > 0 {
					b.push("user", parts)
				}
			}
		}
	}
	if b.contents == nil {
		b.contents = []content{}
	}
	return b.contents, system
}

// mapTools maps the tool definitions: nothing when there are no tools, else one
// tool holding every declaration.
func mapTools(tools []toolhub.Definition) []tool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]functionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description, ParametersJSONSchema: t.Parameters})
	}
	return []tool{{FunctionDeclarations: decls}}
}

// mapGenerationConfig is the generation config of the set options, nil when none
// is set. maxOutputTokens is left out unless set, so the model's own output limit
// applies: Gemini counts thinking tokens against it, and
// llmhub.DefaultMaxOutputTokens could cut a thinking model short.
func mapGenerationConfig(opts llmhub.ModelOptions) *generationConfig {
	g := generationConfig{
		MaxOutputTokens: opts.MaxTokens,
		Temperature:     opts.Temperature,
		TopP:            opts.TopP,
	}
	if len(opts.StopSequences) > 0 {
		g.StopSequences = opts.StopSequences
	}
	if opts.ReasoningEffort != "" {
		g.ThinkingConfig = &thinkingConfig{ThinkingLevel: opts.ReasoningEffort}
	}
	if g.MaxOutputTokens == nil && g.Temperature == nil && g.TopP == nil && g.StopSequences == nil && g.ThinkingConfig == nil {
		return nil
	}
	return &g
}

// buildBody returns the exact request body of one call.
func buildBody(msgs []kernel.Message, opts llmhub.ModelOptions) ([]byte, error) {
	contents, system := mapContents(msgs, opts)
	body := requestBody{
		Contents:         contents,
		Tools:            mapTools(opts.Tools),
		GenerationConfig: mapGenerationConfig(opts),
	}
	if len(system) > 0 {
		body.SystemInstruction = &systemInstruction{Parts: system}
	}
	return jsonx.Marshal(body)
}
