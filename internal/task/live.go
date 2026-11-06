package task

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dethmasque/ackn4th/internal/event"
)

// Live is what an agent's model is producing right now: the streamed
// output events.jsonl leaves out. While a reply streams, the runner
// rewrites it a few times a second, so a dashboard in another process can
// show work in progress, such as a large file being written. It is removed
// once the reply is done and its tool calls and message are in the log.
type Live struct {
	Updated time.Time `json:"updated"`
	// Phase is what the model is writing: "thinking", "replying" or "tool".
	Phase    string `json:"phase"`
	Thinking string `json:"thinking,omitempty"` // the end of its reasoning summary
	Text     string `json:"text,omitempty"`     // the end of its reply text
	Tool     string `json:"tool,omitempty"`     // the tool it is calling
	Path     string `json:"path,omitempty"`     // the call's path argument, once streamed
	// Preview is the end of the longest text the call is writing (a
	// file's content, an edit's new text), decoded from the JSON input.
	Preview string `json:"preview,omitempty"`
	Bytes   int    `json:"bytes,omitempty"` // tool input streamed so far
}

// LivePath is where agent n (0-based) shows its streaming output.
func (s *Store) LivePath(id string, n int) string {
	return filepath.Join(s.Dir(id), "agents", fmt.Sprintf("%d.live.json", n+1))
}

// ReadLive returns agent n's streaming output, or nil when it has none.
func (s *Store) ReadLive(id string, n int) *Live {
	if checkID(id) != nil {
		return nil
	}
	data, err := os.ReadFile(s.LivePath(id, n))
	if err != nil {
		return nil
	}
	var l Live
	if json.Unmarshal(data, &l) != nil {
		return nil
	}
	return &l
}

const (
	liveTail  = 6 << 10 // bytes of each stream kept
	liveHead  = 2 << 10 // bytes of tool input kept from its start, for the path
	liveEvery = 250 * time.Millisecond
)

// liveFeed collects streamed events per agent and writes them out, at most
// every liveEvery per agent.
type liveFeed struct {
	comm  Backend
	mu    sync.Mutex
	feeds map[liveKey]*feed
}

type liveKey struct {
	task  string
	agent int
}

type feed struct {
	live       Live
	head, tail string // tool input
	last       time.Time
	timer      *time.Timer
	dirty      bool
}

func newLiveFeed(b Backend) *liveFeed { return &liveFeed{comm: b, feeds: map[liveKey]*feed{}} }

// Handle folds one event into its agent's feed.
func (lf *liveFeed) Handle(id string, e event.Event) {
	if e.Agent < 1 {
		return
	}
	k := liveKey{id, e.Agent - 1}
	lf.mu.Lock()
	defer lf.mu.Unlock()
	f := lf.feeds[k]
	if !e.Ephemeral() {
		// Once a reply is done its results are in the log: clear the feed.
		switch e.Kind {
		case event.ToolCall, event.Message, event.TurnStarted, event.TurnEnded:
			if f != nil {
				if f.timer != nil {
					f.timer.Stop()
				}
				delete(lf.feeds, k)
				lf.comm.ClearLive(k.task, k.agent)
			}
		}
		return
	}
	if f == nil {
		f = &feed{}
		lf.feeds[k] = f
	}
	l := &f.live
	switch e.Kind {
	case event.Thinking:
		l.Phase, l.Thinking = "thinking", keepTail(l.Thinking+e.Text, liveTail)
	case event.Delta:
		l.Phase, l.Text = "replying", keepTail(l.Text+e.Text, liveTail)
	case event.ToolInput:
		if e.Tool != "" { // a new call
			l.Tool, l.Path, l.Preview, l.Bytes = e.Tool, "", "", 0
			f.head, f.tail = "", ""
		}
		l.Phase = "tool"
		l.Bytes += len(e.Text)
		if len(f.head) < liveHead {
			f.head += e.Text[:min(len(e.Text), liveHead-len(f.head))]
		}
		f.tail = keepTail(f.tail+e.Text, liveTail)
		if l.Path == "" {
			l.Path = cmp.Or(jsonField(f.head, "path"), jsonField(f.tail, "path"))
		}
		l.Preview = previewInput(f.tail, l.Bytes <= len(f.tail))
	}
	f.dirty = true
	if wait := liveEvery - time.Since(f.last); wait > 0 {
		if f.timer == nil {
			f.timer = time.AfterFunc(wait, func() {
				lf.mu.Lock()
				defer lf.mu.Unlock()
				f.timer = nil
				if lf.feeds[k] == f {
					lf.flush(k, f)
				}
			})
		}
		return
	}
	lf.flush(k, f)
}

func (lf *liveFeed) flush(k liveKey, f *feed) {
	if !f.dirty {
		return
	}
	f.dirty, f.last = false, time.Now()
	f.live.Updated = f.last
	lf.comm.WriteLive(k.task, k.agent, f.live)
}

// clear removes every feed's file, when a runner exits.
func (lf *liveFeed) clear(id string, agents int) {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	for n := range agents {
		k := liveKey{id, n}
		if f := lf.feeds[k]; f != nil && f.timer != nil {
			f.timer.Stop()
		}
		delete(lf.feeds, k)
		lf.comm.ClearLive(id, n)
	}
}

// keepTail cuts s to its last n bytes, on a rune boundary.
func keepTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// jsonField finds a complete string field in partial JSON.
func jsonField(s, name string) string {
	m := regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*:\s*("(?:[^"\\]|\\.)*")`).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	v, err := strconv.Unquote(m[1])
	if err != nil {
		return ""
	}
	return v
}

// stringStart matches the start of a JSON string value: an unescaped
// quote, a key, a colon and the value's opening quote.
var stringStart = regexp.MustCompile(`(?:^|[^\\])"([A-Za-z_]+)"\s*:\s*"`)

// previewInput decodes the text a tool call is writing: the longest string
// value in tail, the end of its JSON input, other than the path. whole
// says tail is the entire input; otherwise it may begin mid-value.
func previewInput(tail string, whole bool) string {
	var best string
	if !whole {
		best = unescapePartial(tail)
	}
	for _, m := range stringStart.FindAllStringSubmatchIndex(tail, -1) {
		if tail[m[2]:m[3]] == "path" {
			continue
		}
		if v := unescapePartial(tail[m[1]:]); len(v) > len(best) {
			best = v
		}
	}
	return best
}

// unescapePartial decodes a JSON string body that may be cut off, up to
// its closing quote if it has one.
func unescapePartial(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			break
		}
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(s) {
			break
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteString("    ")
		case 'r':
		case 'u':
			if i+4 < len(s) {
				if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(r))
				}
				i += 4
			}
		default: // \" \\ \/
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
