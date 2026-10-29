package arch_test

import (
	"strings"
	"testing"
)

// vendorAllowed is the dependency policy for the vendor SDKs (Phase 8, CONTEXT
// A2): the non-test code of a package may import an external
// package only when this map lists the package (a module-relative path) and the
// import equals an entry EXACTLY. An entry names one package, never a module
// prefix: a prefix would also admit the packages of the same module that read the
// environment or start a process (review WR-04): aws-sdk-go-v2/config (the default
// credential chain, shared files, IMDS), credentials/processcreds (runs a process),
// credentials/ssocreds and credentials/stscreds, feature/ec2/imds, and
// anthropic-sdk-go/config, /bedrock, /vertex and /foundry (which read AWS_*,
// GOOGLE_* and ANTHROPIC_FOUNDRY_* variables). Every provider leaf may import the
// exact packages it uses today and nothing else, so llm, kernel, tool, persistence,
// the root package and every other leaf stay free of third-party code. A leaf that
// needs one more package adds it here with a stated reason. llm/openaisub is stdlib
// only (raw net/http, CONTEXT A1) and so has no entry, and so is llm/gemini (A-162).
var vendorAllowed = map[string][]string{
	"components/llmhub/providers/anthropic": {
		"github.com/anthropics/anthropic-sdk-go",
		"github.com/anthropics/anthropic-sdk-go/option",
		"github.com/anthropics/anthropic-sdk-go/packages/param",
	},
	"components/llmhub/providers/openaicompat": {
		"github.com/openai/openai-go/v3",
		"github.com/openai/openai-go/v3/option",
		"github.com/openai/openai-go/v3/packages/ssestream",
	},
	"components/llmhub/providers/bedrock": {
		"github.com/aws/aws-sdk-go-v2/aws",
		"github.com/aws/aws-sdk-go-v2/aws/transport/http",
		"github.com/aws/aws-sdk-go-v2/credentials",
		"github.com/aws/aws-sdk-go-v2/service/bedrock",
		"github.com/aws/aws-sdk-go-v2/service/bedrock/types",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types",
		"github.com/aws/smithy-go",
		"github.com/aws/smithy-go/document",
	},
	// A-100 phase 3: Slack's Socket Mode and Discord's Gateway are WebSockets; the client
	// is coder/websocket (already linked by openai-go). Teams is stdlib only.
	"components/channelplatformhub/slack":   {"github.com/coder/websocket"},
	"components/channelplatformhub/discord": {"github.com/coder/websocket"},
}

// testVendorAllowed is the same exact-package policy for test files (a test file
// of a leaf may also use everything vendorAllowed lists for it): the loopback
// replays and the real eventstream encoder need a few more packages, and the
// conformance package may import the AWS packages its Bedrock runner builds an
// injected fake from. goleak stays allowed everywhere through testExternalAllowed.
var testVendorAllowed = map[string][]string{
	"components/llmhub/providers/anthropic": {},
	"components/llmhub/providers/openaicompat": {
		"github.com/openai/openai-go/v3",
		"github.com/openai/openai-go/v3/option",
		"github.com/openai/openai-go/v3/packages/ssestream",
	},
	"components/llmhub/providers/bedrock": {
		"github.com/aws/aws-sdk-go-v2/aws",
		"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream",
		"github.com/aws/aws-sdk-go-v2/aws/transport/http",
		"github.com/aws/aws-sdk-go-v2/service/bedrock",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types",
		"github.com/aws/smithy-go",
		"github.com/aws/smithy-go/transport/http",
	},
	"conformance": {
		"github.com/aws/aws-sdk-go-v2/aws",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime",
		"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types",
		"github.com/aws/smithy-go",
	},
}

// vendorImportAllowed reports whether imp is exactly one of the package paths
// listed for the package rel in policy. There is no prefix match: a package below
// an allowed one, or one that merely starts with the same words, is not allowed
// until it is listed (review WR-04).
func vendorImportAllowed(policy map[string][]string, rel, imp string) bool {
	return contains(policy[rel], imp)
}

// TestLayersCatchVendorImport proves the vendor policy can fail: a vendor import
// in llm, kernel, tool or the root package, and a leaf importing another leaf's
// vendor module, are violations; a leaf importing its own vendor module, and a
// leaf test importing an allowed test vendor, are not.
func TestLayersCatchVendorImport(t *testing.T) {
	const (
		anthropicSDK = "github.com/anthropics/anthropic-sdk-go"
		openaiSDK    = "github.com/openai/openai-go/v3"
		awsSDK       = "github.com/aws/aws-sdk-go-v2"
	)
	for _, pkg := range []string{"components/llmhub", "kernel", "components/toolhub", "components/persistence", "aecmock", "components/llmhub/providers/openaisub", "components/llmhub/providers/gemini", "components/llmhub/providers/all", "."} {
		for _, imp := range []string{anthropicSDK, openaiSDK + "/option", awsSDK + "/service/bedrockruntime", "github.com/aws/smithy-go"} {
			path := archModule + "/" + pkg
			if pkg == "." {
				path = archModule
			}
			v := checkLayers([]pkgInfo{{ImportPath: path, Imports: []string{imp}}}, layerTable)
			if len(v) != 1 || !strings.Contains(v[0], "external import") || !strings.Contains(v[0], imp) {
				t.Errorf("%s importing %s: want one external import violation, got %v", pkg, imp, v)
			}
		}
	}
	// A leaf may not import another leaf's vendor module.
	for leaf, foreign := range map[string]string{
		"components/llmhub/providers/anthropic":    openaiSDK,
		"components/llmhub/providers/openaicompat": anthropicSDK,
		"components/llmhub/providers/bedrock":      anthropicSDK,
	} {
		if v := checkLayers([]pkgInfo{synth(leaf, foreign)}, layerTable); len(v) != 1 || !strings.Contains(v[0], "external import") {
			t.Errorf("%s importing %s: got %v", leaf, foreign, v)
		}
	}
	// A leaf may import the exact packages listed for it.
	for leaf, own := range map[string][]string{
		"components/llmhub/providers/anthropic":    {anthropicSDK, anthropicSDK + "/option"},
		"components/llmhub/providers/openaicompat": {openaiSDK, openaiSDK + "/option", openaiSDK + "/packages/ssestream"},
		"components/llmhub/providers/bedrock":      {awsSDK + "/service/bedrockruntime", awsSDK + "/aws", "github.com/aws/smithy-go", "github.com/aws/smithy-go/document"},
	} {
		if v := checkLayers([]pkgInfo{synth(leaf, own...)}, layerTable); len(v) != 0 {
			t.Errorf("%s importing its own vendor module: got %v", leaf, v)
		}
	}
	// A module whose path only starts like an allowed one is not matched.
	if v := checkLayers([]pkgInfo{synth("components/llmhub/providers/anthropic", anthropicSDK+"-fork")}, layerTable); len(v) != 1 {
		t.Errorf("a look-alike module path was accepted: %v", v)
	}
	// Test files: a leaf test may use its own vendor module and goleak, not another's.
	okTest := checkLayers([]pkgInfo{{
		ImportPath:   archModule + "/components/llmhub/providers/bedrock",
		TestImports:  []string{awsSDK + "/service/bedrockruntime/types", "go.uber.org/goleak"},
		XTestImports: []string{"github.com/aws/smithy-go"},
	}}, layerTable)
	if len(okTest) != 0 {
		t.Errorf("a bedrock test importing the AWS modules was flagged: %v", okTest)
	}
	badTest := checkLayers([]pkgInfo{{ImportPath: archModule + "/components/llmhub/providers/anthropic", TestImports: []string{openaiSDK}}}, layerTable)
	if len(badTest) != 1 || !strings.Contains(badTest[0], "external test import") {
		t.Errorf("a foreign vendor module in a leaf test was not reported: %v", badTest)
	}
	llmTest := checkLayers([]pkgInfo{{ImportPath: archModule + "/components/llmhub", TestImports: []string{anthropicSDK}}}, layerTable)
	if len(llmTest) != 1 {
		t.Errorf("a vendor module in an llm test was not reported: %v", llmTest)
	}
	// The Bedrock conformance runner builds an injected fake from the AWS modules.
	conf := checkLayers([]pkgInfo{{ImportPath: archModule + "/conformance", TestImports: []string{awsSDK + "/service/bedrockruntime"}}}, layerTable)
	if len(conf) != 0 {
		t.Errorf("a conformance test importing the AWS modules was flagged: %v", conf)
	}
	// The same import in non-test code of conformance is still a violation.
	if v := checkLayers([]pkgInfo{synth("conformance", awsSDK)}, layerTable); len(v) != 1 {
		t.Errorf("an AWS import in conformance non-test code was accepted: %v", v)
	}
}

// TestLayersCatchWiderVendorPackages proves the vendor policy is exact (review
// WR-04): a package of the leaf's own vendor module that is not listed is a
// violation, in non-test and in test code. The packages named here are the ones
// that read the environment, run a process or load shared files: the default
// credential chain and its providers of the AWS SDK, and the cloud-platform
// wrappers of the Anthropic SDK; an unlisted harmless package shows that the rule is
// "listed", not "dangerous".
func TestLayersCatchWiderVendorPackages(t *testing.T) {
	const (
		anthropicSDK = "github.com/anthropics/anthropic-sdk-go"
		openaiSDK    = "github.com/openai/openai-go/v3"
		awsSDK       = "github.com/aws/aws-sdk-go-v2"
	)
	wider := map[string][]string{
		"components/llmhub/providers/bedrock": {
			awsSDK + "/config", awsSDK + "/credentials/processcreds", awsSDK + "/credentials/ssocreds",
			awsSDK + "/credentials/stscreds", awsSDK + "/credentials/endpointcreds", awsSDK + "/feature/ec2/imds",
			awsSDK + "/service/sts", awsSDK + "/service/sso", awsSDK + "/service/ssooidc", awsSDK + "/aws/retry",
		},
		"components/llmhub/providers/anthropic": {
			anthropicSDK + "/config", anthropicSDK + "/bedrock", anthropicSDK + "/vertex",
			anthropicSDK + "/foundry", anthropicSDK + "/packages/ssestream",
		},
		"components/llmhub/providers/openaicompat": {openaiSDK + "/azure", openaiSDK + "/packages/param", openaiSDK + "/shared"},
	}
	for leaf, imps := range wider {
		for _, imp := range imps {
			if v := checkLayers([]pkgInfo{synth(leaf, imp)}, layerTable); len(v) != 1 || !strings.Contains(v[0], "external import") || !strings.Contains(v[0], imp) {
				t.Errorf("%s importing %s: want one external import violation, got %v", leaf, imp, v)
			}
			test := checkLayers([]pkgInfo{{ImportPath: archModule + "/" + leaf, TestImports: []string{imp}}}, layerTable)
			if len(test) != 1 || !strings.Contains(test[0], "external test import") {
				t.Errorf("%s test importing %s: want one external test import violation, got %v", leaf, imp, test)
			}
		}
	}
	if v := checkLayers([]pkgInfo{{ImportPath: archModule + "/conformance", TestImports: []string{awsSDK + "/config"}}}, layerTable); len(v) != 1 {
		t.Errorf("conformance test importing the AWS config package: got %v", v)
	}
	// The listed packages stay allowed, so the guard is not simply a ban.
	for leaf, imps := range vendorAllowed {
		if v := checkLayers([]pkgInfo{synth(leaf, imps...)}, layerTable); len(v) != 0 {
			t.Errorf("%s importing its listed packages: got %v", leaf, v)
		}
	}
}
