package ax

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/mattn/go-runewidth"
	"golang.org/x/term"
)

// The inbox is a local observer. Neither of these RPCs binds an agent,
// acknowledges a message, or participates in the delivery lifecycle.
type inboxItem struct {
	ID        string `json:"message_id"`
	Sender    string `json:"sender"`
	Recipient string `json:"recipient"`
	State     string `json:"status"`
	Preview   string `json:"preview"`
	Created   int64  `json:"created_at_ms"`
	Thread    string `json:"thread_id"`
}

type InboxFilter struct{ Target, Thread, State string }

func parseInboxFilter(args []string) (InboxFilter, error) {
	var filter InboxFilter
	flags := flag.NewFlagSet("inbox", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&filter.Target, "a", "", "Agent name")
	flags.StringVar(&filter.Thread, "t", "", "Thread ID")
	flags.StringVar(&filter.State, "s", "", "Delivery state")
	if err := flags.Parse(args); err != nil {
		return filter, err
	}
	if flags.NArg() > 1 || (flags.NArg() == 1 && filter.Target != "") {
		return filter, errors.New("usage: ax inbox [-a NAME] [-t THREAD_ID] [-s STATE] [NAME]")
	}
	if flags.NArg() == 1 {
		filter.Target = flags.Arg(0)
	}
	return filter, filter.validate()
}

func (f InboxFilter) validate() error {
	if f.Thread != "" && !validID.MatchString(f.Thread) {
		return errors.New("invalid thread ID")
	}
	if f.State != "" {
		switch f.State {
		case "queued", "handoff_started", "wake_accepted", "channel_written", "content_served", "acknowledged", "delivery_uncertain", "expired", "refused", "abandoned":
		default:
			return errors.New("unknown delivery state")
		}
	}
	return nil
}

func (b *broker) inbox(target string, filters ...InboxFilter) ([]inboxItem, error) {
	var options InboxFilter
	if len(filters) > 0 {
		options = filters[0]
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	filter := ""
	if target != "" {
		p, e := b.resolve(target)
		if e != nil {
			return nil, e
		}
		filter = p.ID
	}
	// Bound the result and frame size. Bodies are fetched individually.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	rows, e := b.db.QueryContext(ctx, `SELECT m.id,json_extract(s.data,'$.name') || CASE WHEN json_extract(s.data,'$.kind')='relay' THEN ' [relay]' ELSE '' END,json_extract(r.data,'$.name'),m.state,m.created,
 substr(json_extract(m.data,'$.text'),1,160),coalesce(json_extract(m.data,'$.thread_id'),'') FROM messages m
 JOIN agents s ON s.id=m.sender JOIN agents r ON r.id=m.recipient
 WHERE (?='' OR m.sender=? OR m.recipient=?) AND (?='' OR json_extract(m.data,'$.thread_id')=?)
 AND (?='' OR m.state=?) ORDER BY m.rowid DESC LIMIT 100`, filter, filter, filter, options.Thread, options.Thread, options.State, options.State)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	items := []inboxItem{}
	for rows.Next() {
		var item inboxItem
		if e = rows.Scan(&item.ID, &item.Sender, &item.Recipient, &item.State, &item.Created, &item.Preview, &item.Thread); e != nil {
			return nil, e
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type inboxView struct {
	items    []inboxItem
	selected int
	scroll   int
	body     Message
	detail   inboxDetail
	problem  string
}

func (v *inboxView) selectItems(items []inboxItem) {
	previous := ""
	if len(v.items) > 0 {
		previous = v.items[v.selected].ID
	}
	v.items = items
	v.selected = 0
	for i, item := range items {
		if item.ID == previous {
			v.selected = i
			return
		}
	}
	v.scroll = 0
}

func (v *inboxView) move(delta int) {
	if len(v.items) > 0 {
		v.selected = max(0, min(len(v.items)-1, v.selected+delta))
		v.scroll = 0
	}
}

// Message text is data, including escape codes, bidi controls, and OSC sequences.
// Only this renderer's own fixed escape sequences may reach the terminal.
func inboxText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

func (v *inboxView) render(width, height int, target string) string {
	width = max(1, width-1) // Avoid wrapping at the terminal's right edge.
	height = max(1, height)
	lines := []string{"AX INBOX  ·  read only", "Latest 100 matches · terminal mail retained up to 7 days · receipt is not task completion"}
	if target != "" {
		lines[0] += "  ·  " + target
	}
	if v.problem != "" {
		lines[1] = "Offline or unavailable · " + v.problem + " · retrying every 2s"
	}
	listHeight := max(1, (height-7)/2)
	start := max(0, v.selected-listHeight+1)
	for i := start; i < min(len(v.items), start+listHeight); i++ {
		item := v.items[i]
		mark := "  "
		if i == v.selected {
			mark = "> "
		}
		lines = append(lines, fmt.Sprintf("%s%s  %s → %s  [%s]  %s", mark,
			time.UnixMilli(item.Created).Format("15:04"), item.Sender, item.Recipient,
			item.State, strings.ReplaceAll(item.Preview, "\n", " ")))
	}
	if len(v.items) == 0 {
		lines = append(lines, "No retained messages match. Older or expired history may have been removed.")
	}
	for len(lines) < 2+listHeight {
		lines = append(lines, "")
	}
	lines = append(lines, strings.Repeat("─", width))
	if len(v.items) > 0 {
		item := v.items[v.selected]
		lines = append(lines, item.Sender+" → "+item.Recipient+"  ["+item.State+"]")
		body := "Loading message…"
		if v.body.ID == item.ID {
			body = v.body.Text
		}
		if v.detail.Message.ID == item.ID {
			body = v.detail.text(time.Now())
		}
		wrapped := strings.Split(runewidth.Wrap(inboxText(body), width), "\n")
		bodyHeight := max(1, height-len(lines)-2)
		v.scroll = min(v.scroll, max(0, len(wrapped)-bodyHeight))
		lines = append(lines, wrapped[v.scroll:min(len(wrapped), v.scroll+bodyHeight)]...)
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	lines = append(lines, "↑/↓ j/k select · u/d scroll body · g newest · q quit")
	lines = lines[:min(len(lines), height)]
	for i, line := range lines {
		lines[i] = runewidth.Truncate(inboxText(line), width, "…")
	}
	return "\x1b[H" + strings.Join(lines, "\x1b[K\r\n") + "\x1b[K\x1b[J"
}

// The observer owns only its own socket and terminal. It never starts or stops
// the broker or a harness, including while reconnecting after a broker restart.
func Inbox(ctx context.Context, dir, target string, in, out *os.File) error {
	return InboxFiltered(ctx, dir, InboxFilter{Target: target}, in, out)
}

func InboxFiltered(ctx context.Context, dir string, filter InboxFilter, in, out *os.File) error {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return errors.New("ax inbox needs an interactive terminal; use ax agents or ax status MESSAGE_ID in scripts")
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGHUP)
	defer cancel()
	state, e := term.MakeRaw(int(in.Fd()))
	if e != nil {
		return e
	}
	defer term.Restore(int(in.Fd()), state)
	if _, e = io.WriteString(out, "\x1b[?1049h\x1b[?25l"); e != nil {
		return e
	}
	defer io.WriteString(out, "\x1b[?25h\x1b[?1049l")
	keys := make(chan byte, 16)
	go func() {
		defer close(keys)
		var b [1]byte
		for {
			if _, e := in.Read(b[:]); e != nil {
				return
			}
			select {
			case keys <- b[0]:
			case <-ctx.Done():
				return
			}
		}
	}()
	view := inboxView{problem: "connecting"}
	// One worker, one request at a time, with a fixed backoff even on immediate
	// errors. Keeping I/O off the render loop makes quit responsive during outages.
	type update struct {
		items  []inboxItem
		body   Message
		err    error
		detail inboxDetail
	}
	updates := make(chan update, 1)
	selection := make(chan string, 1)
	go func() {
		var c *client
		defer func() {
			if c != nil {
				c.close()
			}
		}()
		id := ""
		for ctx.Err() == nil {
			var u update
			if c == nil {
				c, u.err = dial(socketPath(dir))
			}
			if u.err == nil {
				u.err = c.callContext(ctx, "ax.inbox", object{"target": filter.Target, "thread_id": filter.Thread, "delivery_state": filter.State}, &u.items)
			}
			if u.err == nil {
				for _, item := range u.items {
					if item.ID == id {
						// Local inspection does not mark mail read by its recipient.
						u.err = c.callContext(ctx, "ax.inspect_message", object{"message_id": id}, &u.detail)
						u.body = u.detail.Message
						break
					}
				}
			}
			if u.err != nil && c != nil {
				c.close()
				c = nil
			}
			select {
			case updates <- u:
			case <-ctx.Done():
				return
			}
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case next := <-selection:
				id = next
				// A selection may refresh immediately only on a healthy connection.
				if u.err != nil {
					select {
					case <-ctx.Done():
					case <-timer.C:
					}
				}
				timer.Stop()
			}
		}
	}()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	requested, previousFrame := "", ""
	escape := 0
	for {
		if len(view.items) > 0 && view.items[view.selected].ID != requested {
			requested = view.items[view.selected].ID
			select {
			case <-selection:
			default:
			}
			selection <- requested
		}
		width, height, e := term.GetSize(int(out.Fd()))
		if e != nil {
			return e
		}
		label := filter.Target
		if filter.Thread != "" {
			label += " thread=" + filter.Thread
		}
		if filter.State != "" {
			label += " state=" + filter.State
		}
		frame := view.render(width, height, label)
		if frame != previousFrame {
			if _, e = io.WriteString(out, frame); e != nil {
				return e
			}
			previousFrame = frame
		}
		select {
		case <-ctx.Done():
			return nil
		case <-resize:
		case u := <-updates:
			if u.err != nil {
				view.problem = u.err.Error()
			} else {
				view.problem = ""
				view.selectItems(u.items)
				view.body = u.body
				view.detail = u.detail
			}
		case key, ok := <-keys:
			if !ok || key == 'q' || key == 3 || key == 4 {
				return nil
			}
			if key == 27 {
				escape = 1
				continue
			}
			if escape == 1 && (key == '[' || key == 'O') {
				escape = 2
				continue
			}
			if escape == 2 {
				switch key {
				case 'A':
					view.move(-1)
				case 'B':
					view.move(1)
				}
			} else {
				switch key {
				case 'j':
					view.move(1)
				case 'k':
					view.move(-1)
				case 'u':
					view.scroll = max(0, view.scroll-3)
				case 'd':
					view.scroll += 3
				case 'g':
					view.move(-view.selected)
				}
			}
			escape = 0
		}
	}
}
