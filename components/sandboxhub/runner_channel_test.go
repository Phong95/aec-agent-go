package sandboxhub

import (
	"encoding/json"
	"io"
	"testing"
	"time"
)

// A-160: the channel ends and the server side's reading of a core/ts runner.

// A core/ts runner sends Node's mtimeMs as it is (with a fraction): the Go server still
// reads the stat, cut to whole milliseconds.
func TestRunnerReadsFractionalMillisecondsFromATSRunner(t *testing.T) {
	var sb *RunnerSandbox
	sb = NewRunnerSandbox(RunnerSandboxOptions{
		ID:    "remote:runner:s1",
		Hello: RunnerHello{Type: RunnerMessageHello, Protocol: 1, OS: OSWindows, FolderName: "site"},
		Send: func(m RunnerMessage) error {
			if m.Type != RunnerMessageRequest {
				return nil
			}
			go sb.Receive([]byte(`{"type":"response","id":"` + m.ID + `","result":{"path":"/workspace/a.txt","type":"file","size":3,"modifiedAt":1792927034982.5643}}`))
			return nil
		},
	})
	view, err := sb.ForAgent(t.Context(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	st, err := view.Stat(t.Context(), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st == nil || st.ModifiedAt != 1792927034982 || st.Size != 3 {
		t.Fatalf("stat = %+v", st)
	}
}

func TestChannelReaderAnswersEachFrameWhenTheNextIsAskedFor(t *testing.T) {
	acks := 0
	r := NewChannelReader(func() { acks++ }, nil)
	r.Push("aGk=") // "hi"
	r.Push("IHRoZXJl")
	r.End()
	buf := make([]byte, 2)
	if n, err := r.Read(buf); err != nil || string(buf[:n]) != "hi" || acks != 0 {
		t.Fatalf("read %q %v, acks %d", buf[:n], err, acks)
	}
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != " there" || acks != 2 {
		t.Fatalf("rest %q %v, acks %d", rest, err, acks)
	}
}

func TestChannelWriterWaitsForTheWindow(t *testing.T) {
	sent := make(chan RunnerMessage, 100)
	w := NewChannelWriter(func(m RunnerMessage) { sent <- m })
	done := make(chan error, 1)
	go func() {
		_, err := w.Write(make([]byte, RunnerStreamChunkBytes*(RunnerStreamWindow+2)))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if len(sent) != RunnerStreamWindow {
		t.Fatalf("%d frames before any ack, want %d", len(sent), RunnerStreamWindow)
	}
	w.OnAck()
	w.OnAck()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	first := <-sent
	if b, _ := json.Marshal(first); len(b) == 0 || first.Type != RunnerMessageData {
		t.Fatalf("frame = %+v", first)
	}
}
