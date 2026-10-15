package openaisub

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Phong95/aec-agent-go/components/llmhub"
	"github.com/Phong95/aec-agent-go/internal/llmtest"
	"github.com/Phong95/aec-agent-go/kernel"
)

// TestUpdateTokensKeepsPairsConsistent is the proof of CONTEXT D1 and of success
// criterion 4: UpdateTokens swaps the access token and the account id as one pair,
// while requests are in flight.
//
// Sixteen goroutines each send 25 streams (and 10 model lists) through one provider
// while another goroutine calls UpdateTokens("tok-k", "acct-k") over and over, for 8
// values of k. The upstream fails the test when the Authorization header of a request
// ends in another k than its ChatGPT-Account-Id header: a token paired with the
// account id of another UpdateTokens call. Run under go test -race, a read of the pair
// without the lock is also reported as a data race. With the lock removed from
// UpdateTokens and snapshot this test fails (the mutation check of the SUMMARY).
func TestUpdateTokensKeepsPairsConsistent(t *testing.T) {
	const (
		workers   = 16
		streams   = 25
		listings  = 10
		variants  = 8
		tokPrefix = "Bearer tok-"
	)

	var (
		mu   sync.Mutex
		torn []string
		seen = map[string]int{}
	)
	up := llmtest.NewUpstream(t, func(r llmtest.Request) llmtest.Reply {
		k := strings.TrimPrefix(r.Header.Get("Authorization"), tokPrefix)
		account := strings.TrimPrefix(r.Header.Get("ChatGPT-Account-Id"), "acct-")
		mu.Lock()
		seen[k]++
		if k != account || !strings.HasPrefix(r.Header.Get("Authorization"), tokPrefix) {
			torn = append(torn, fmt.Sprintf("Authorization %q with ChatGPT-Account-Id %q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-Id")))
		}
		mu.Unlock()
		if r.Path == modelsPath {
			return llmtest.Reply{Header: map[string]string{"Content-Type": "application/json"}, Chunks: []string{`{"models":[]}`}}
		}
		return sseBytes([]string{minimalStream})
	})
	p := newTestProvider(t, up, caseConfig{Token: "tok-0", Account: "acct-0"})

	// The updater runs until every worker is done.
	stop := make(chan struct{})
	var updater sync.WaitGroup
	updater.Add(1)
	go func() {
		defer updater.Done()
		for k := 0; ; k++ {
			select {
			case <-stop:
				return
			default:
			}
			n := k % variants
			p.UpdateTokens(fmt.Sprintf("tok-%d", n), fmt.Sprintf("acct-%d", n))
		}
	}()

	var failures sync.Map
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range streams {
				var last kernel.StreamChunk
				for c, err := range p.Stream(t.Context(), helloMsgs, llmhub.ModelOptions{}) {
					if err != nil {
						failures.Store(fmt.Sprintf("worker %d stream %d: %v", w, i, err), true)
						return
					}
					last = c
				}
				if last.Type != kernel.ChunkDone {
					failures.Store(fmt.Sprintf("worker %d stream %d ended with %+v", w, i, last), true)
				}
			}
			for i := range listings {
				if _, err := p.ListModels(t.Context()); err != nil {
					failures.Store(fmt.Sprintf("worker %d listing %d: %v", w, i, err), true)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	updater.Wait()

	failures.Range(func(k, _ any) bool {
		t.Errorf("a call failed: %v", k)
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	if len(torn) > 0 {
		t.Fatalf("%d of %d requests carried a mismatched pair, for example %s", len(torn), workers*(streams+listings), torn[0])
	}
	total := 0
	for _, n := range seen {
		total += n
	}
	if total != workers*(streams+listings) {
		t.Errorf("the upstream saw %d requests, want %d", total, workers*(streams+listings))
	}
	if len(seen) < 2 {
		t.Errorf("only %d distinct token values reached the upstream: UpdateTokens did not interleave with the requests", len(seen))
	}
}
