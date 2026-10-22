package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Recorder appends one NDJSON line per frame to the transcript. This file is
// witness B of the suite's three witnesses: the record of what the client
// actually put on the wire, independent of anything the client reports about
// itself.
type Recorder struct {
	mu  sync.Mutex
	f   *os.File
	seq int
}

// Frame is the transcript line format. Method and ID are lifted out of the
// payload so tests can filter without re-parsing every line.
type Frame struct {
	Seq    int             `json:"seq"`
	At     string          `json:"at"`
	Dir    string          `json:"dir"` // "in" = client -> fixture, "out" = fixture -> client
	Method string          `json:"method,omitempty"`
	ID     json.RawMessage `json:"id,omitempty"`
	Raw    string          `json:"raw"`
	Note   string          `json:"note,omitempty"`
	Extra  map[string]any  `json:"extra,omitempty"`
}

func NewRecorder(path string) (*Recorder, error) {
	if path == "" {
		return &Recorder{}, nil // recording disabled
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Recorder{f: f}, nil
}

func (r *Recorder) write(fr Frame) {
	if r == nil || r.f == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	fr.Seq = r.seq
	fr.At = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(fr)
	if err != nil {
		return
	}
	r.f.Write(append(b, '\n'))
	r.f.Sync() // a crash fault must not lose the frames that led up to it
}

// Raw records a frame verbatim, pulling out method and id on a best effort
// basis: a malformed frame still gets recorded, just without those fields.
func (r *Recorder) Raw(dir string, b []byte) {
	fr := Frame{Dir: dir, Raw: string(b)}
	var probe struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
	}
	if json.Unmarshal(b, &probe) == nil {
		fr.Method, fr.ID = probe.Method, probe.ID
	}
	r.write(fr)
}

// Note records something that happened but did not cross the wire, such as the
// fixture deliberately exiting or hanging. It makes a transcript that ends
// abruptly self-explanatory.
func (r *Recorder) Note(note string, extra map[string]any) {
	r.write(Frame{Dir: "note", Note: note, Extra: extra})
}

func (r *Recorder) Close() {
	if r != nil && r.f != nil {
		r.f.Close()
	}
}
