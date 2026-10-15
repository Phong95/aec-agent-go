package job

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Phong95/aec-agent-go/internal/jsstr"
)

// ---------------------------------------------------------------------------
// Webhook deliveries for triggers (A-70): the signing secret and the signature checks
// on the raw bytes. Update (A-126): for webhook jobs (a trigger is now a job with
// When.Kind "webhook").
//
//   - "standard" - Standard Webhooks (standardwebhooks.com): webhook-id,
//     webhook-timestamp and webhook-signature ("v1,<base64>", space separated);
//     HMAC-SHA256 over "<id>.<timestamp>.<body>" with the base64-decoded secret; the
//     timestamp must be within WebhookTolerance, so a captured request cannot be
//     replayed later.
//   - "github" - X-Hub-Signature-256 ("sha256=<hex>"), HMAC-SHA256 over the body with
//     the secret's text. GitHub signs no timestamp, so none is enforced;
//     X-GitHub-Delivery is the event id (a redelivery keeps it, so it dedupes), and
//     X-GitHub-Event (+ the body's "action") is the type.
//
// Comparisons are constant-time (hmac.Equal). Nothing here fails for a bad request: the
// result says what is wrong, for a 4xx answer.
// ---------------------------------------------------------------------------

const secretPrefix = "whsec_"

// GenerateWebhookSecret returns a fresh signing secret: "whsec_" + 32 random bytes in
// base64.
func GenerateWebhookSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // Go 1.24+: Read never returns an error
	return secretPrefix + base64.StdEncoding.EncodeToString(b)
}

// SignStandardWebhook signs a Standard Webhooks delivery - what a sender does (tests,
// the docs' example). timestamp is in seconds.
func SignStandardWebhook(secret, id string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, standardKey(secret))
	mac.Write([]byte(id + "." + strconv.FormatInt(timestamp, 10) + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// SignGithubWebhook signs a GitHub-style delivery: the X-Hub-Signature-256 value.
func SignGithubWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature returns "" when the request's signature is valid under scheme
// and secret, otherwise why it is not. now is the time the timestamp is checked against.
func VerifyWebhookSignature(scheme SignatureScheme, secret string, req WebhookRequest, now time.Time) string {
	if scheme == SignatureGithub {
		header := Header(req, "x-hub-signature-256")
		if !strings.HasPrefix(header, "sha256=") {
			return "missing X-Hub-Signature-256"
		}
		if !safeEqual(header, SignGithubWebhook(secret, req.Body)) {
			return "the signature does not match"
		}
		return ""
	}
	id := Header(req, "webhook-id")
	timestamp := Header(req, "webhook-timestamp")
	signatures := Header(req, "webhook-signature")
	if id == "" || timestamp == "" || signatures == "" {
		return "missing webhook-id, webhook-timestamp or webhook-signature"
	}
	seconds, err := strconv.ParseFloat(jsstr.Trim(timestamp), 64)
	if err != nil || math.IsInf(seconds, 0) || math.IsNaN(seconds) || seconds != math.Trunc(seconds) || math.Abs(seconds) > 1e15 {
		return "webhook-timestamp is not a number of seconds"
	}
	if math.Abs(float64(now.UnixMilli())-seconds*1000) > float64(WebhookTolerance.Milliseconds()) {
		return "webhook-timestamp is too old or in the future"
	}
	expected := SignStandardWebhook(secret, id, int64(seconds), req.Body)
	for s := range strings.SplitSeq(signatures, " ") {
		if strings.HasPrefix(s, "v1,") && safeEqual(s, expected) {
			return ""
		}
	}
	return "the signature does not match"
}

// Header returns the request header name (lower-case), also when the host passed it in
// another case; "" when absent.
func Header(req WebhookRequest, name string) string {
	if v, ok := req.Headers[name]; ok {
		return v
	}
	for k, v := range req.Headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// standardKey is the secret's key bytes: the base64 after "whsec_", decoded as leniently
// as Node's Buffer.from(s, "base64") does (URL-safe letters accepted, padding and other
// characters ignored).
func standardKey(secret string) []byte {
	s := strings.TrimPrefix(secret, secretPrefix)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '+', r == '/':
			b.WriteRune(r)
		case r == '-':
			b.WriteByte('+')
		case r == '_':
			b.WriteByte('/')
		}
	}
	clean := b.String()
	if len(clean)%4 == 1 {
		clean = clean[:len(clean)-1]
	}
	key, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return nil
	}
	return key
}

func safeEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}
