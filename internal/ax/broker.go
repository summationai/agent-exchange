package ax

import (
	"cmp"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

var validName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,47}$`)
var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

type Agent struct {
	ID           string                `json:"agent_id"`
	Name         string                `json:"name"`
	Host         string                `json:"host"`
	Mesh         string                `json:"mesh"`
	Native       string                `json:"native_session_id,omitempty"`
	Permission   string                `json:"permission_mode"`
	State        string                `json:"state"`
	Policy       string                `json:"policy"`
	Online       bool                  `json:"online"`
	AllowBypass  bool                  `json:"allow_bypass"`
	BindingError string                `json:"binding_error,omitempty"`
	Capabilities *deliveryCapabilities `json:"delivery_capabilities,omitempty"`
	DeliveryMode string                `json:"delivery_mode,omitempty"`
	Connectivity agentCapabilities     `json:"capabilities"`
}
type Message struct {
	HandoffID     string           `json:"handoff_id,omitempty"` // Offer-only, never stored in the mailbox.
	ID            string           `json:"message_id"`
	Sender        Agent            `json:"sender"`
	Recipient     string           `json:"recipient_id"`
	Text          string           `json:"text"`
	Parent        string           `json:"in_reply_to,omitempty"`
	ResendOf      string           `json:"resend_of,omitempty"`
	Thread        string           `json:"thread_id,omitempty"`
	Seq           int64            `json:"recipient_seq"`
	Created       int64            `json:"created_at_ms"`
	Expires       int64            `json:"expires_at_ms"`
	Depth         int              `json:"reply_depth"`
	State         string           `json:"status"`
	Receipt       *sendReceipt     `json:"receipt,omitempty"`
	QueuedContext *deliveryContext `json:"queued_context,omitempty"`
}
type peer struct {
	handoffID, handoffMessage string
	Agent
	hash         string
	epoch        int64
	seen         time.Time
	conn         *serverConn
	ready        bool
	enrolled     time.Time
	notice       string
	noticeRetry  time.Time
	capabilities *deliveryCapabilities
}
type serverConn struct {
	net.Conn
	writeMu         sync.Mutex
	agent           string
	epoch           int64
	lifecycleEvents bool
}

func (c *serverConn) send(p packet) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.SetWriteDeadline(time.Now().Add(time.Second))
	return writeFrame(c, p)
}

type broker struct {
	mu          sync.Mutex
	db          *sql.DB
	peers       map[string]*peer
	lastCleanup time.Time
	dir         string
	lastReap    time.Time
}

func openBroker(dir string) (*broker, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := filepath.Join(dir, "mailbox.db"+suffix)
		if suffix == "" {
			f, e := privateFile(p, syscall.O_CREAT|syscall.O_RDWR)
			if e != nil {
				return nil, e
			}
			f.Close()
		} else if _, e := os.Lstat(p); e == nil {
			f, e := privateFile(p, syscall.O_RDWR)
			if e != nil {
				return nil, e
			}
			f.Close()
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(dir, "mailbox.db")}
	db, e := sql.Open("sqlite3", uri.String()+"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*broker, error) { db.Close(); return nil, e }
	var version int
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		return fail(e)
	}
	if version > 3 {
		return fail(errors.New("database version is newer than this AX binary"))
	}
	_, e = db.Exec(`BEGIN;
 CREATE TABLE IF NOT EXISTS agents(id TEXT PRIMARY KEY, hash TEXT NOT NULL, data TEXT NOT NULL, epoch INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY,sender TEXT NOT NULL REFERENCES agents(id),recipient TEXT NOT NULL REFERENCES agents(id),client_id TEXT NOT NULL,request_hash TEXT NOT NULL,seq INTEGER NOT NULL,state TEXT NOT NULL,data TEXT NOT NULL,created INTEGER NOT NULL,expires INTEGER NOT NULL, UNIQUE(sender,client_id),UNIQUE(recipient,seq));
 CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY,message TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,state TEXT NOT NULL,at INTEGER NOT NULL,epoch INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS mailbox ON messages(recipient,seq);
 CREATE INDEX IF NOT EXISTS sender_time ON messages(sender,created);
 CREATE INDEX IF NOT EXISTS receipt_history ON events(message,id);
 CREATE INDEX IF NOT EXISTS terminal_retention ON messages(created) WHERE state IN ('acknowledged','expired','refused','abandoned');
 CREATE INDEX IF NOT EXISTS pending_mailbox ON messages(recipient,seq) WHERE state IN ('queued','handoff_started','delivery_uncertain');
 CREATE INDEX IF NOT EXISTS agent_names ON agents(json_extract(data,'$.name'));
 CREATE INDEX IF NOT EXISTS active_agents ON agents(id) WHERE json_extract(data,'$.state')!='exited';
 CREATE INDEX IF NOT EXISTS message_parent ON messages(coalesce(json_extract(data,'$.in_reply_to'),json_extract(data,'$.resend_of')),json_extract(data,'$.thread_id'));
 CREATE INDEX IF NOT EXISTS message_thread ON messages(json_extract(data,'$.thread_id'));
 CREATE INDEX IF NOT EXISTS queued_expiry ON messages(expires) WHERE state='queued';
 COMMIT;`)
	if e != nil {
		return fail(e)
	}
	if version < 2 {
		_, e = db.Exec(`BEGIN; ALTER TABLE agents ADD COLUMN next_seq INTEGER NOT NULL DEFAULT 0; UPDATE agents SET next_seq=coalesce((SELECT max(seq) FROM messages WHERE recipient=agents.id),0); PRAGMA user_version=2; COMMIT;`)
		if e != nil {
			return fail(e)
		}
	}
	if _, e = db.Exec(`BEGIN;
 CREATE TABLE IF NOT EXISTS notifications(id TEXT PRIMARY KEY,recipient TEXT NOT NULL REFERENCES agents(id),message TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,data TEXT NOT NULL,acknowledged INTEGER NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS pending_notifications ON notifications(recipient) WHERE acknowledged=0;
 PRAGMA user_version=3; COMMIT;`); e != nil {
		return fail(e)
	}
	var mode string
	if e = db.QueryRow("PRAGMA journal_mode").Scan(&mode); e != nil {
		return fail(e)
	}
	if mode != "wal" {
		return fail(errors.New("SQLite WAL unavailable"))
	}
	b := &broker{db: db, dir: dir, peers: map[string]*peer{}}
	rows, e := db.Query("SELECT hash,data,epoch FROM agents WHERE json_extract(data,'$.state')!='exited'")
	if e != nil {
		return fail(e)
	}
	for rows.Next() {
		p := &peer{}
		var data string
		if e = rows.Scan(&p.hash, &data, &p.epoch); e != nil {
			rows.Close()
			return fail(e)
		}
		if e = json.Unmarshal([]byte(data), &p.Agent); e != nil {
			rows.Close()
			return fail(e)
		}
		p.Online = false
		p.enrolled = time.Now()
		b.peers[p.ID] = p
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return fail(e)
	}
	_, e = db.Exec(`BEGIN; INSERT INTO events(message,state,at,epoch) SELECT id,'delivery_uncertain',?,0 FROM messages WHERE state='handoff_started'; UPDATE messages SET state='delivery_uncertain' WHERE state='handoff_started'; COMMIT;`, time.Now().UnixMilli())
	if e != nil {
		return fail(e)
	}
	// Older brokers downgraded confirmed handoffs when disconnecting, including
	// during an upgrade. Restore only recorded proof after the last handoff; this
	// releases later mail without retrying the already accepted message.
	_, e = db.Exec(`UPDATE messages SET state=(
 SELECT state FROM events WHERE message=messages.id AND state IN ('wake_accepted','channel_written','content_served')
 AND id>coalesce((SELECT max(id) FROM events WHERE message=messages.id AND state='handoff_started'),0)
 ORDER BY (state='content_served') DESC,id DESC LIMIT 1)
 WHERE state='delivery_uncertain' AND EXISTS (
 SELECT 1 FROM events WHERE message=messages.id AND state IN ('wake_accepted','channel_written','content_served')
 AND id>coalesce((SELECT max(id) FROM events WHERE message=messages.id AND state='handoff_started'),0));`)
	if e != nil {
		return fail(e)
	}
	b.reapSessions(time.Now())
	return b, nil
}
func Serve(ctx context.Context, dir string) error {
	l, lock, e := listenLocal(dir)
	if e != nil {
		return e
	}
	defer lock.Close()
	defer l.Close()
	b, e := openBroker(dir)
	if e != nil {
		return e
	}
	defer b.db.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-done:
		}
	}()
	defer close(done)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				b.mu.Lock()
				b.reapSessions(time.Now())
				b.cleanup(time.Now())
				b.dispatch()
				b.mu.Unlock()
			}
		}
	}()
	var wg sync.WaitGroup
	var cm sync.Mutex
	conns := map[*serverConn]bool{}
	defer func() {
		cm.Lock()
		for c := range conns {
			c.Close()
		}
		cm.Unlock()
		wg.Wait()
	}()
	for {
		c, e := l.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		u, ok := c.(*net.UnixConn)
		if !ok || !sameUID(u) {
			c.Close()
			continue
		}
		cm.Lock()
		if len(conns) >= 128 {
			cm.Unlock()
			c.Close()
			continue
		}
		sc := &serverConn{Conn: c}
		conns[sc] = true
		cm.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { cm.Lock(); delete(conns, sc); cm.Unlock() }()
			defer sc.Close()
			defer b.disconnect(sc)
			for {
				sc.SetReadDeadline(time.Now().Add(20 * time.Second))
				p, e := readFrame(sc)
				if e != nil {
					return
				}
				if len(p.ID) == 0 {
					return
				}
				b.mu.Lock()
				result, e := b.request(sc, p.Method, p.Params)
				b.mu.Unlock()
				reply := packet{ID: p.ID}
				if e != nil {
					reply.Error = &rpcError{Code: -32000, Message: e.Error()}
					var rpc *rpcError
					if errors.As(e, &rpc) {
						reply.Error = rpc
					}
				} else {
					reply.Result = raw(result)
				}
				if sc.send(reply) != nil {
					return
				}
				if !dispatchAfter(p.Method) {
					continue
				}
				b.mu.Lock()
				b.dispatch()
				b.mu.Unlock()
			}
		}()
	}
}
func (b *broker) save(p *peer) error {
	_, e := b.db.Exec("INSERT INTO agents(id,hash,data,epoch) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data,epoch=excluded.epoch", p.ID, p.hash, string(raw(p.Agent)), p.epoch)
	return e
}
func (b *broker) event(id, state string, epoch int64) error {
	tx, e := b.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	storedState := state
	if state == "deferred_idle" {
		storedState = "queued"
	}
	if _, e = tx.Exec("UPDATE messages SET state=? WHERE id=?", storedState, id); e != nil {
		return e
	}
	at := time.Now().UnixMilli()
	if _, e = tx.Exec("INSERT INTO events(message,state,at,epoch) VALUES(?,?,?,?)", id, state, at, epoch); e != nil {
		return e
	}
	if e = failureNotice(tx, id, state, at); e != nil {
		return e
	}
	return tx.Commit()
}
func (b *broker) disconnect(c *serverConn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.peers[c.agent]
	if p == nil || p.conn != c {
		return
	}
	p.conn = nil
	p.Online = false
	b.uncertain(p.ID, p.epoch)
}
func (b *broker) uncertain(id string, epoch int64) {
	_, _ = b.db.Exec(`BEGIN; INSERT INTO events(message,state,at,epoch) SELECT id,'delivery_uncertain',?,? FROM messages WHERE recipient=? AND state='handoff_started'; UPDATE messages SET state='delivery_uncertain' WHERE recipient=? AND state='handoff_started'; COMMIT;`, time.Now().UnixMilli(), epoch, id, id)
}
func (b *broker) auth(c *serverConn) (*peer, error) {
	p := b.peers[c.agent]
	if p == nil || p.conn != c || p.epoch != c.epoch || time.Since(p.seen) > 15*time.Second {
		return nil, errors.New("inactive or fenced AX session")
	}
	return p, nil
}
func (b *broker) request(c *serverConn, method string, params json.RawMessage) (any, error) {
	var a struct {
		Session
		HandoffID       string                `json:"handoff_id"`
		ThreadFilter    string                `json:"thread_id"`
		StateFilter     string                `json:"delivery_state"`
		Version         string                `json:"version"`
		Native          string                `json:"native_session_id"`
		Permission      string                `json:"permission_mode"`
		State           string                `json:"state"`
		Policy          string                `json:"policy"`
		Target          string                `json:"target"`
		Text            string                `json:"text"`
		ClientID        string                `json:"client_message_id"`
		MessageID       string                `json:"message_id"`
		AfterMessage    string                `json:"after_message_id"`
		NotificationID  string                `json:"notification_id"`
		Receipt         string                `json:"receipt"`
		TTL             json.RawMessage       `json:"ttl_seconds"`
		AfterSeq        json.RawMessage       `json:"after_seq"`
		Mesh            string                `json:"mesh"`
		LifecycleEvents bool                  `json:"lifecycle_events"`
		Capabilities    *deliveryCapabilities `json:"delivery_capabilities"`
	}
	if e := json.Unmarshal(params, &a); e != nil {
		return nil, errors.New("invalid parameters")
	}
	if method == "ax.ping" {
		return object{"version": "1", "local_attach": true}, nil
	}
	if method == "ax.enroll" {
		s := a.Session
		s.Mesh = a.Mesh
		s.Native = a.Native
		if !validName.MatchString(s.Name) || harnesses[s.Host].nativeID == nil || !validID.MatchString(s.ID) || len(s.Secret) < 32 || len(s.Mesh) != 24 {
			return nil, errors.New("invalid session registration")
		}
		if s.Host == "external" && (s.DeliveryMode != "manual" && s.DeliveryMode != "adapter") || s.Host != "external" && s.DeliveryMode != "" {
			return nil, errors.New("invalid delivery mode")
		}
		p := b.peers[s.ID]
		if p == nil {
			if len(b.peers) >= 256 {
				return nil, errors.New("endpoint limit reached")
			}
			var e error
			p, e = storedPeer(b.db, s.ID)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return nil, e
			}
		}
		if p != nil {
			if subtle.ConstantTimeCompare([]byte(p.hash), []byte(digest(s.Secret))) != 1 {
				return nil, errors.New("invalid session credential")
			}
			if p.Name != s.Name || p.Host != s.Host {
				return nil, errors.New("saved identity belongs to another name or harness")
			}
			p.State = "starting"
			p.BindingError = ""
			p.Permission = "unknown"
			p.AllowBypass = s.AllowBypass
			p.Mesh = s.Mesh
			p.DeliveryMode = s.DeliveryMode
			if e := b.save(p); e != nil {
				return nil, e
			}
			p.enrolled = time.Now()
			b.peers[p.ID] = p
			return p.Agent, nil
		}
		var names int
		if e := b.db.QueryRow("SELECT count(*) FROM agents WHERE json_extract(data,'$.name')=?", s.Name).Scan(&names); e != nil {
			return nil, e
		}
		if names != 0 {
			return nil, errors.New("name already in use; resume its saved AX identity")
		}
		p = &peer{Agent: Agent{ID: s.ID, Name: s.Name, Host: s.Host, Mesh: s.Mesh, Native: s.Native, State: "starting", Permission: "unknown", Policy: "accept", AllowBypass: s.AllowBypass, DeliveryMode: s.DeliveryMode}, hash: digest(s.Secret)}
		if e := b.save(p); e != nil {
			return nil, e
		}
		b.peers[p.ID] = p
		p.enrolled = time.Now()
		return p.Agent, nil
	}
	if method == "ax.connect" {
		if a.Version != "1" {
			return nil, errors.New("unsupported AX protocol version")
		}
		p := b.peers[a.ID]
		if p == nil || subtle.ConstantTimeCompare([]byte(p.hash), []byte(digest(a.Secret))) != 1 {
			return nil, errors.New("invalid session credential")
		}
		if c.agent != "" {
			return nil, errors.New("connection already registered")
		}
		if err := validateCapabilities(p.Host, a.Capabilities); err != nil {
			return nil, err
		}
		// Duplicate MCP processes must not fence and reconnect each other forever.
		// Preserve the live owner; a disconnected or expired lease can be replaced.
		if p.conn != nil && time.Since(p.seen) <= 15*time.Second {
			return nil, errors.New("AX agent already connected; waiting for its existing bridge to disconnect")
		}
		for _, other := range b.peers {
			if other.ID != p.ID && other.Name == p.Name && other.Online {
				return nil, errors.New("name already in use")
			}
		}
		p.epoch++
		if e := b.save(p); e != nil {
			p.epoch--
			return nil, e
		}
		b.uncertain(p.ID, p.epoch-1)
		if p.conn != nil {
			p.conn.Close()
		}
		p.conn = c
		p.notice = ""
		p.noticeRetry = time.Time{}
		p.ready = false
		p.seen = time.Now()
		p.Online = true
		c.agent = p.ID
		c.epoch = p.epoch
		c.lifecycleEvents = a.LifecycleEvents
		p.capabilities = a.Capabilities
		p.handoffID, p.handoffMessage = "", ""
		return object{"agent_id": p.ID, "lease_epoch": p.epoch, "delivery_capabilities": p.capabilities}, nil
	}
	if method == "ax.lifecycle" && c.agent == "" {
		p := b.peers[a.ID]
		if p == nil || subtle.ConstantTimeCompare([]byte(p.hash), []byte(digest(a.Secret))) != 1 {
			return nil, errors.New("invalid lifecycle credential")
		}
		before := p.Agent
		// Native resume/picker selection chooses the ID after enrollment. The
		// credentialed lifecycle hook may establish its first binding.
		if p.Native == "" && nativeID(p.Host, a.Native) {
			p.Native = a.Native
		}
		if a.Native != p.Native {
			return nil, errors.New("hook belongs to another session")
		}
		if a.BindingError != "" {
			if len(a.BindingError) > 1024 {
				return nil, errors.New("binding error is too long")
			}
			p.BindingError, p.State = a.BindingError, "blocked"
		}
		if a.Permission != "" {
			p.Permission = a.Permission
		}
		if p.BindingError == "" && (a.State == "ready" || a.State == "busy" || a.State == "blocked") {
			p.State = a.State
		}
		if p.Agent == before {
			return object{"ok": true}, nil
		}
		if err := b.save(p); err != nil {
			p.Agent = before
			return nil, err
		}
		if p.conn != nil && p.conn.lifecycleEvents && !p.ready {
			// Optional hint for this bridge only. Old clients retain heartbeat setup.
			if p.conn.send(packet{Method: "ax.lifecycle.changed"}) != nil {
				p.conn.Close()
			}
		}
		return object{"ok": true}, nil
	}
	// Local administration is available only on unbound connections, never via MCP.
	if c.agent == "" && method == "ax.inbox" {
		return b.inbox(a.Target, InboxFilter{Thread: a.ThreadFilter, State: a.StateFilter})
	}
	if c.agent == "" && method == "ax.inbox_message" {
		return b.message(a.MessageID)
	}
	if c.agent == "" && method == "ax.inspect" {
		return b.list(), nil
	}
	if c.agent == "" && method == "ax.inspect_message" {
		return b.status(a.MessageID)
	}
	if c.agent == "" && method == "ax.abandon" {
		m, e := b.message(a.MessageID)
		if e != nil {
			return nil, e
		}
		if m.State == "acknowledged" || m.State == "expired" || m.State == "refused" || m.State == "abandoned" {
			return nil, errors.New("message is already terminal")
		}
		if m.State == "queued" {
			return nil, errors.New("message has not been handed off; queued mail cannot be abandoned by recovery")
		}
		// A human can release a stuck FIFO head without claiming receipt or
		// repeating a potentially successful host injection.
		if e = b.event(m.ID, "abandoned", 0); e != nil {
			return nil, e
		}
		return b.status(m.ID)
	}
	if c.agent == "" && method == "ax.policy" {
		p, e := b.resolve(a.Target)
		if e != nil {
			return nil, e
		}
		if a.Policy != "accept" && a.Policy != "hold" && a.Policy != "refuse" {
			return nil, errors.New("policy must be accept, hold, or refuse")
		}
		p.Policy = a.Policy
		return p.Agent, b.save(p)
	}
	p, e := b.auth(c)
	if e != nil {
		return nil, e
	}
	switch method {
	case "ax.capabilities":
		if err := validateCapabilities(p.Host, a.Capabilities); err != nil {
			return nil, err
		}
		p.capabilities = a.Capabilities
		return object{"delivery_capabilities": p.capabilities}, nil
	case "ax.notifications":
		return b.notices(p.ID)
	case "ax.ack_notification":
		return b.ackNotice(p, a.NotificationID)
	case "ax.notice_retry":
		if p.notice == a.NotificationID {
			p.notice = ""
			p.noticeRetry = time.Now().Add(30 * time.Second)
		}
		return object{"ok": true}, nil
	case "ax.ready":
		if p.BindingError != "" {
			return nil, errors.New(p.BindingError)
		}
		p.ready = true
		return object{"ok": true}, nil
	case "ax.heartbeat":
		p.seen = time.Now()
		return object{"ok": true}, nil
	case "ax.presence":
		if a.BindingError != "" {
			if len(a.BindingError) > 1024 {
				return nil, errors.New("binding error is too long")
			}
			p.BindingError, p.State = a.BindingError, "blocked"
			return p.Agent, b.save(p)
		}
		if p.BindingError != "" {
			return nil, errors.New(p.BindingError)
		}
		if a.Native != "" {
			if !nativeID(p.Host, a.Native) {
				return nil, errors.New("invalid native session ID")
			}
			if p.Native != "" && p.Native != a.Native {
				return nil, errors.New("native session changed; use a new AX name")
			}
			p.Native = a.Native
		}
		if a.Permission != "" {
			p.Permission = a.Permission
		}
		if a.State != "" {
			if a.State != "ready" && a.State != "busy" && a.State != "blocked" && a.State != "starting" {
				return nil, errors.New("invalid presence state")
			}
			p.State = a.State
		}
		p.seen = time.Now()
		return p.Agent, b.save(p)
	case "ax.list":
		return b.list(), nil
	case "ax.pending":
		after, err := integerArgument(a.AfterSeq, "after_seq", 0, 0, maxSequence)
		if err != nil {
			return nil, err
		}
		return b.pending(p, after)
	case "ax.check_inbox":
		return b.checkInbox(p)
	case "ax.thread":
		return b.thread(p, a.MessageID, a.AfterMessage)
	case "ax.send", "ax.reply", "ax.resend", "ax.follow_up":
		ttl, err := integerArgument(a.TTL, "ttl_seconds", defaultTTL, 1, maxTTL)
		if err != nil {
			return nil, err
		}
		return b.send(p, a.Target, a.MessageID, a.Text, a.ClientID, int(ttl), method)
	case "ax.get_message", "ax.ack", "ax.receipt", "ax.status":
		m, e := b.message(a.MessageID)
		if e != nil {
			return nil, e
		}
		if method == "ax.status" {
			if m.Sender.ID != p.ID && m.Recipient != p.ID {
				return nil, errors.New("message not accessible")
			}
			return b.status(m.ID)
		}
		if m.Recipient != p.ID {
			return nil, errors.New("message is addressed to another endpoint")
		}
		if method == "ax.receipt" && a.Receipt == "deferred_idle" {
			if p.capabilities == nil || p.capabilities.Boundary != "idle" || a.HandoffID == "" ||
				a.HandoffID != p.handoffID || m.ID != p.handoffMessage {
				return nil, errors.New("idle deferral requires the current unaccepted handoff")
			}
			// Native idle may already have followed the busy preflight. Preserve
			// that newer state so normal dispatch can retry this definite non-send.
			if m.State == "queued" {
				return object{"ok": true}, nil
			} // Duplicate of this attempt's deferral.
			if m.State != "handoff_started" {
				return nil, errors.New("cannot defer an accepted or uncertain handoff")
			}
			return object{"ok": true}, b.event(m.ID, "deferred_idle", p.epoch)
		}
		if m.State == "queued" || m.State == "expired" || m.State == "refused" || m.State == "abandoned" {
			return nil, errors.New("message has not been offered")
		}
		if method == "ax.get_message" {
			if m.State != "acknowledged" && m.State != "content_served" {
				if e = b.event(m.ID, "content_served", p.epoch); e != nil {
					return nil, e
				}
			}
			return m, nil
		}
		state := a.Receipt
		if method == "ax.ack" {
			state = "acknowledged"
		}
		if state != "acknowledged" && state != "wake_accepted" && state != "channel_written" && state != "delivery_uncertain" {
			return nil, errors.New("invalid receipt")
		}
		// A fast recipient may fetch before the bridge records its host receipt.
		// Never let that late receipt regress stronger evidence of delivery.
		if (state == "wake_accepted" || state == "channel_written") && (m.State == "content_served" || m.State == "acknowledged") {
			// Keep the timing evidence even when state has already advanced. At
			// most one observation of each receipt is retained, including retries.
			_, err := b.db.Exec(`INSERT INTO events(message,state,at,epoch)
 SELECT ?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM events WHERE message=? AND state=?)`,
				m.ID, state, time.Now().UnixMilli(), p.epoch, m.ID, state)
			return object{"ok": true}, err
		}
		if m.State == "acknowledged" || m.State == "abandoned" || m.State == state || (m.State == "content_served" && state != "acknowledged") {
			return object{"ok": true}, nil
		}
		return object{"ok": true}, b.event(m.ID, state, p.epoch)
	}
	return nil, errors.New("unknown AX method")
}
func validNative(id string) bool {
	return regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(id)
}

// Lead with connected peers, ahead of sessions whose bridge is reconnecting.
func (b *broker) list() []Agent {
	out := []Agent{}
	for _, p := range b.peers {
		out = append(out, agentSnapshot(p))
	}
	rank := func(a Agent) int {
		if a.Online {
			return 0
		}
		return 1
	}
	slices.SortFunc(out, func(x, y Agent) int {
		return cmp.Or(rank(x)-rank(y), strings.Compare(x.Name, y.Name))
	})
	return out
}
func (b *broker) resolve(target string) (*peer, error) {
	var matches []*peer
	for _, p := range b.peers {
		if p.ID == target || p.Name == target {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return b.savedTarget(target)
	}
	if len(matches) != 1 {
		return nil, errors.New("ambiguous agent name; list agents and use agent_id")
	}
	return matches[0], nil
}
func (b *broker) message(id string) (Message, error) {
	var m Message
	var data, state string
	e := b.db.QueryRow("SELECT data,state FROM messages WHERE id=?", id).Scan(&data, &state)
	if e != nil {
		return m, errors.New("message not found")
	}
	if e = json.Unmarshal([]byte(data), &m); e != nil {
		return m, e
	}
	m.State = state
	return m, nil
}
func (b *broker) status(id string) (any, error) {
	m, e := b.message(id)
	if e != nil {
		return nil, e
	}
	rows, e := b.db.Query("SELECT state,at,epoch FROM events WHERE message=? ORDER BY id", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	events := []object{}
	for rows.Next() {
		var s string
		var at, epoch int64
		if e = rows.Scan(&s, &at, &epoch); e != nil {
			return nil, e
		}
		events = append(events, object{"event": s, "at_ms": at, "epoch": epoch})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // Release the single DB connection before reading current state.
	snapshot, err := b.deliverySnapshot(m)
	if err != nil {
		return nil, err
	}
	return object{"message": m, "events": events, "timing": messageTiming(m, events), "current_snapshot": snapshot, "delivery_evidence": deliveryEvidence(m.State), "acknowledged": m.State == "acknowledged", "task_completion": "unknown"}, nil
}
func safe(p *peer) bool {
	if p.Host == "pi" && p.Permission == "native" {
		// Pi has no built-in approval mode. Its configured extensions and
		// execution environment retain control over all native tool calls.
		return true
	}
	if p.AllowBypass {
		return true
	}
	switch p.Permission {
	case "default", "acceptEdits", "plan", "auto", "dontAsk", "read-only", "workspace-write":
		return true
	}
	return false
}
func (b *broker) send(p *peer, target, parent, text, key string, ttl int, method string) (any, error) {
	reply, resend, followup := method == "ax.reply", method == "ax.resend", method == "ax.follow_up"
	if !validID.MatchString(key) || (!resend && (!utf8.ValidString(text) || len(text) == 0 || len(text) > maxText)) {
		return nil, errors.New("provide client_message_id and 1 to 65536 bytes of UTF-8 text")
	}
	if ttl == 0 {
		ttl = defaultTTL
	}
	if ttl < 1 || ttl > maxTTL {
		return nil, fmt.Errorf("TTL must be between 1 and %d seconds", maxTTL)
	}
	hash := digest(string(raw([]any{target, parent, text, ttl, reply})))
	if resend {
		hash = digest(string(raw([]any{method, parent, ttl})))
	} else if followup {
		hash = digest(string(raw([]any{method, parent, text, ttl})))
	}
	var existing, prior string
	e := b.db.QueryRow("SELECT id,request_hash FROM messages WHERE sender=? AND client_id=?", p.ID, key).Scan(&existing, &prior)
	if e == nil {
		if hash != prior {
			return nil, errors.New("client_message_id already used with different content")
		}
		m, err := b.message(existing)
		if err != nil {
			return nil, err
		}
		return b.sendResult(b.db, m, key)
	}
	if e != sql.ErrNoRows {
		return nil, e
	}
	var to *peer
	depth := 0
	resendOf, thread := "", ""
	if resend {
		m, err := b.message(parent)
		if err != nil {
			return nil, err
		}
		if m.Sender.ID != p.ID {
			return nil, errors.New("only the original sender can resend a message")
		}
		if m.State != "expired" {
			return nil, errors.New("only expired messages can be resent; never replay accepted or uncertain work")
		}
		to, e = b.messagePeer(b.db, m.Recipient)
		if e != nil {
			return nil, e
		}
		text, parent, depth, resendOf = m.Text, m.Parent, m.Depth, m.ID
		thread, e = b.threadRoot(m)
		if e != nil {
			return nil, e
		}
	} else if reply || followup {
		m, e := b.message(parent)
		if e != nil {
			return nil, e
		}
		if (reply && m.Recipient != p.ID) || (followup && m.Sender.ID != p.ID) {
			return nil, errors.New("only the recipient may reply; only the sender may follow up")
		}
		if (reply && m.State == "queued") || m.State == "expired" || m.State == "refused" || m.State == "abandoned" {
			return nil, errors.New("message is not eligible for a reply or follow-up")
		}
		targetID := m.Sender.ID
		if followup {
			targetID = m.Recipient
		}
		to, e = b.messagePeer(b.db, targetID)
		if e != nil {
			return nil, e
		}
		thread, e = b.threadRoot(m)
		if e != nil {
			return nil, e
		}
		depth = m.Depth + 1
		if depth > 8 {
			recovery := fmt.Sprintf("If the user has authorized continuing in a fresh thread, including a standing instruction, call send_message with target=%q. Send the pending handoff with a concise summary and previous message ID %s, using context already available. Do not ask again or reread files merely to switch threads. This does not authorize replaying completed work or continuing acknowledgment loops.", to.ID, parent)
			if reply {
				recovery += " After the new send succeeds, call ack_message for the previous message, then end your turn."
			} else {
				recovery += " End your turn after sending; do not acknowledge your own outgoing message."
			}
			return nil, &rpcError{Code: replyDepthLimitCode, Message: "Reply depth limit reached; this message was not sent. " + recovery, Data: object{
				"target": to.ID, "previous_message_id": parent, "thread_id": thread, "recovery": recovery,
			}}
		}
	} else {
		to, e = b.resolve(target)
		if e != nil {
			return nil, e
		}
		parent = ""
	}
	if to == nil || to.ID == p.ID {
		return nil, errors.New("invalid recipient")
	}
	if to.Policy == "refuse" {
		return nil, errors.New("recipient refuses messages")
	}
	var count, size int64
	if e = b.db.QueryRow("SELECT count(*),coalesce(sum(length(data)),0) FROM messages WHERE state NOT IN ('acknowledged','expired','refused','abandoned')").Scan(&count, &size); e != nil {
		return nil, e
	}
	if count >= 1024 || size >= 32<<20 {
		return nil, errors.New("mailbox capacity reached")
	}
	if e = b.db.QueryRow("SELECT count(*) FROM messages WHERE sender=? AND created>?", p.ID, time.Now().Add(-time.Minute).UnixMilli()).Scan(&count); e != nil {
		return nil, e
	}
	if count >= 30 {
		return nil, errors.New("send rate limit reached")
	}
	if e = b.db.QueryRow("SELECT count(*) FROM messages WHERE sender=? AND recipient=? AND json_extract(data,'$.text')=? AND created>? AND id!=?", p.ID, to.ID, text, time.Now().Add(-10*time.Second).UnixMilli(), resendOf).Scan(&count); e != nil {
		return nil, e
	}
	if count > 0 {
		return nil, errors.New("repeated message suppressed; retry with the original client_message_id")
	}
	tx, e := b.db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var seq int64
	if e = tx.QueryRow("UPDATE agents SET next_seq=next_seq+1 WHERE id=? RETURNING next_seq", to.ID).Scan(&seq); e != nil {
		return nil, e
	}
	now := time.Now()
	id := randomID("msg_")
	if thread == "" {
		thread = id
	}
	m := Message{ID: id, Thread: thread, Sender: p.Agent, Recipient: to.ID, Text: text, Parent: parent, ResendOf: resendOf, Seq: seq, Created: now.UnixMilli(), Expires: now.Add(time.Duration(ttl) * time.Second).UnixMilli(), Depth: depth, State: "queued"}
	m.QueuedContext = &deliveryContext{BrokerVersion: Version, RecipientHost: to.Host, RecipientState: agentSnapshot(to).State, SenderState: agentSnapshot(p).State}
	if _, e = tx.Exec("INSERT INTO messages VALUES(?,?,?,?,?,?,?,?,?,?)", m.ID, p.ID, to.ID, key, hash, seq, m.State, string(raw(m)), m.Created, m.Expires); e != nil {
		return nil, e
	}
	for _, state := range []string{"stored", "queued"} {
		if _, e = tx.Exec("INSERT INTO events(message,state,at,epoch) VALUES(?,?,?,?)", m.ID, state, m.Created, p.epoch); e != nil {
			return nil, e
		}
	}
	if reply {
		if _, e = tx.Exec("UPDATE messages SET state='acknowledged' WHERE id=?", parent); e != nil {
			return nil, e
		}
		if _, e = tx.Exec("INSERT INTO events(message,state,at,epoch) VALUES(?,?,?,?)", parent, "replied", m.Created, p.epoch); e != nil {
			return nil, e
		}
	}
	m, e = b.sendResult(tx, m, key)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return m, nil
}

// FIFO orders host handoffs, not task completion. Once a host accepts a wake
// (or the recipient fetches it), later mail can enter its native queue even if
// the agent forgets to acknowledge or spends hours working on the first task.
func (b *broker) dispatch() {
	now := time.Now()
	for _, p := range b.peers {
		if p.conn != nil && now.Sub(p.seen) > 15*time.Second {
			p.conn.Close()
			p.conn = nil
			p.Online = false
			b.uncertain(p.ID, p.epoch)
		}
		b.dispatchNotice(p, now)
		for {
			var id, state string
			var expires int64
			e := b.db.QueryRow("SELECT id,state,expires FROM messages WHERE recipient=? AND state IN ('queued','handoff_started','delivery_uncertain') ORDER BY seq LIMIT 1", p.ID).Scan(&id, &state, &expires)
			if e != nil || state != "queued" {
				break
			}
			terminal := ""
			if expires <= now.UnixMilli() {
				terminal = "expired"
			} else if p.Policy == "refuse" {
				terminal = "refused"
			}
			if terminal != "" {
				if b.event(id, terminal, p.epoch) != nil {
					break
				}
				continue
			}
			if p.DeliveryMode == "manual" || p.conn == nil || !p.ready || p.State == "starting" || p.State == "blocked" || p.Native == "" || p.Policy != "accept" || !safe(p) || boundaryWait(p) {
				break
			}
			m, e := b.message(id)
			if e != nil {
				break
			}
			sender, e := b.messagePeer(b.db, m.Sender.ID)
			if e != nil || !safe(sender) {
				break
			}
			if b.event(id, "handoff_started", p.epoch) != nil {
				break
			}
			m.State = "handoff_started"
			m.HandoffID = randomID("handoff_")
			p.handoffID, p.handoffMessage = m.HandoffID, m.ID
			if p.conn.send(packet{Method: "ax.delivery.offer", Params: raw(m)}) != nil {
				b.event(id, "delivery_uncertain", p.epoch)
				p.conn.Close()
			}
			break
		}
	}
}

func dispatchAfter(method string) bool {
	switch method {
	case "ax.ping", "ax.heartbeat", "ax.list", "ax.pending", "ax.thread", "ax.inspect", "ax.status", "ax.inspect_message", "ax.inbox", "ax.inbox_message", "ax.notifications":
		return false
	}
	return true
}

func (b *broker) cleanup(now time.Time) {
	if now.Sub(b.lastCleanup) < time.Minute {
		return
	}
	b.lastCleanup = now
	b.expireArchived(now)
	// Bound each transaction and keep cleanup off request and heartbeat paths.
	_, _ = b.db.Exec(`DELETE FROM messages WHERE id IN (
 SELECT id FROM messages WHERE created<? AND state IN ('acknowledged','expired','refused','abandoned')
 ORDER BY created LIMIT 256)`, now.Add(-7*24*time.Hour).UnixMilli())
}
