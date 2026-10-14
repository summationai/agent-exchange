//go:build darwin || linux

package ax

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const networkA2APath = "/ax/network/v1/a2a/"
const networkAXExtension = "https://useax.dev/extensions/named-mailbox/v1"

// Optional streaming, push, and extended cards are rejected by the SDK's
// capability checks. AX implements the durable non-streaming operations below.
type networkA2AHandler struct {
	a2asrv.RequestHandler
	dir, alias, recipient, protocol, extensions, fingerprint string
}

func networkAgentCard(identity networkCard, agent networkAgent) *a2a.AgentCard {
	return &a2a.AgentCard{
		Name: agent.Name, Description: "A named AX coding session on a paired machine. Messaging preserves native sandbox and approvals.", Version: Version,
		SupportedInterfaces: []*a2a.AgentInterface{{URL: identity.Address + networkA2APath + agent.ID, ProtocolBinding: "JSONRPC", ProtocolVersion: a2a.Version}},
		SecuritySchemes:     a2a.NamedSecuritySchemes{"paired-machine": a2a.MutualTLSSecurityScheme{}}, SecurityRequirements: a2a.SecurityRequirementsOptions{{"paired-machine": {}}},
		DefaultInputModes: []string{"text/plain"}, DefaultOutputModes: []string{"text/plain"},
		Capabilities: a2a.AgentCapabilities{Extensions: []a2a.AgentExtension{{URI: networkAXExtension, Required: true, Description: "Pinned-installation named mailbox identity, absolute expiry, parent mapping, and provenance. Mutual TLS pairing is required."}}},
		Skills:       []a2a.AgentSkill{{ID: "coordinate", Name: "Coordinate", Description: "Send requests and replies to this existing coding session.", Tags: []string{"coding", "coordination"}}},
	}
}

func networkA2AEndpoint(w http.ResponseWriter, r *http.Request, dir string, identity networkCard, paired networkPeer, agents []Agent) bool {
	if !strings.HasPrefix(r.URL.Path, networkA2APath) {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, networkA2APath)
	id := strings.TrimSuffix(path, "/agent-card.json")
	if !validID.MatchString(id) || !paired.Messaging {
		http.Error(w, "recipient is not authorized", http.StatusForbidden)
		return true
	}
	var target *Agent
	for i := range agents {
		if agents[i].ID == id && agents[i].Remote == nil && (slices.Contains(paired.Exports, "*") || slices.Contains(paired.Exports, agents[i].Name)) {
			target = &agents[i]
			break
		}
	}
	if target == nil {
		http.Error(w, "recipient is not authorized", http.StatusForbidden)
		return true
	}
	if path != id {
		if path != id+"/agent-card.json" || r.Method != http.MethodGet || r.ContentLength != 0 {
			http.Error(w, "unsupported operation", http.StatusNotFound)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(networkAgentCard(identity, networkAgent{ID: id, Name: target.Name, Host: target.Host, State: target.State}))
		return true
	}
	peers, err := networkPeers(dir)
	if err != nil {
		http.Error(w, "pairing unavailable", http.StatusServiceUnavailable)
		return true
	}
	alias := ""
	for name, p := range peers {
		if networkFingerprint(p.Card) == networkFingerprint(paired.Card) {
			alias = name
			break
		}
	}
	if alias == "" {
		http.Error(w, "peer is not authorized", http.StatusForbidden)
		return true
	}
	if r.Method != http.MethodPost {
		http.Error(w, "unsupported operation", http.StatusMethodNotAllowed)
		return true
	}
	h := &networkA2AHandler{RequestHandler: a2asrv.NewHandler(nil, a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{})), dir: dir, alias: alias, recipient: id, protocol: r.Header.Get(a2a.SvcParamVersion), extensions: r.Header.Get(a2a.SvcParamExtensions), fingerprint: networkFingerprint(paired.Card)}
	r.Body = http.MaxBytesReader(w, r.Body, maxFrame)
	a2asrv.NewJSONRPCHandler(h).ServeHTTP(w, r)
	return true
}

func (h *networkA2AHandler) checkVersion() error {
	if h.protocol != string(a2a.Version) {
		return a2a.ErrVersionNotSupported
	}
	return nil
}

func (h *networkA2AHandler) call(ctx context.Context, method string, args object, out any) error {
	c, err := dial(socketPath(h.dir))
	if err != nil {
		return a2a.ErrInternalError
	}
	defer c.close()
	args["alias"], args["recipient_id"], args["fingerprint"] = h.alias, h.recipient, h.fingerprint
	if err = c.callContext(ctx, method, args, out); err != nil {
		return a2a.NewError(a2a.ErrInvalidParams, err.Error())
	}
	return nil
}

func networkTaskState(m Message) a2a.TaskState {
	switch m.TaskOutcome {
	case "completed":
		return a2a.TaskStateCompleted
	case "failed":
		return a2a.TaskStateFailed
	case "canceled":
		return a2a.TaskStateCanceled
	case "input_required":
		return a2a.TaskStateInputRequired
	case "working":
		return a2a.TaskStateWorking
	}
	switch m.State {
	case "queued", "handoff_started", "delivery_uncertain", "wake_accepted", "channel_written":
		return a2a.TaskStateSubmitted
	case "expired", "refused", "abandoned":
		return a2a.TaskStateRejected
	default:
		return a2a.TaskStateWorking
	}
}
func networkTaskView(m Message, history *int) *a2a.Task {
	t := &a2a.Task{ID: a2a.TaskID(m.ID), ContextID: m.Thread, Status: a2a.TaskStatus{State: networkTaskState(m)}, Metadata: map[string]any{"ax_delivery": m.State, "ax_message_id": m.ID}}
	if history == nil || *history > 0 {
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(m.Text))
		msg.ID = m.NetworkOrigin.MessageID
		msg.TaskID = t.ID
		msg.ContextID = t.ContextID
		t.History = []*a2a.Message{msg}
	}
	return t
}
func (h *networkA2AHandler) GetTask(ctx context.Context, req *a2a.GetTaskRequest) (*a2a.Task, error) {
	if err := h.checkVersion(); err != nil {
		return nil, err
	}
	if req == nil || req.Tenant != "" || !validID.MatchString(string(req.ID)) || (req.HistoryLength != nil && (*req.HistoryLength < 0 || *req.HistoryLength > 20)) {
		return nil, a2a.ErrInvalidParams
	}
	var m Message
	if err := h.call(ctx, "ax.network.task", object{"message_id": req.ID}, &m); err != nil {
		return nil, a2a.ErrTaskNotFound
	}
	return networkTaskView(m, req.HistoryLength), nil
}
func (h *networkA2AHandler) ListTasks(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if err := h.checkVersion(); err != nil {
		return nil, err
	}
	if req == nil || req.Tenant != "" || req.ContextID != "" || req.Status != "" || req.StatusTimestampAfter != nil || req.IncludeArtifacts || req.PageSize < 0 || req.PageSize > 50 || (req.HistoryLength != nil && (*req.HistoryLength < 0 || *req.HistoryLength > 20)) {
		return nil, a2a.ErrUnsupportedOperation
	}
	var page networkTaskPage
	if err := h.call(ctx, "ax.network.tasks", object{"after": req.PageToken}, &page); err != nil {
		return nil, err
	}
	size := req.PageSize
	if size == 0 {
		size = 50
	}
	out := &a2a.ListTasksResponse{Tasks: []*a2a.Task{}, PageSize: size}
	for _, m := range page.Messages[:min(len(page.Messages), size)] {
		out.Tasks = append(out.Tasks, networkTaskView(m, req.HistoryLength))
	}
	out.TotalSize = page.Total
	if len(page.Messages) > size || page.Next != "" {
		out.NextPageToken = string(out.Tasks[len(out.Tasks)-1].ID)
	}
	return out, nil
}
func (h *networkA2AHandler) CancelTask(ctx context.Context, req *a2a.CancelTaskRequest) (*a2a.Task, error) {
	if err := h.checkVersion(); err != nil {
		return nil, err
	}
	if req == nil || req.Tenant != "" || !validID.MatchString(string(req.ID)) {
		return nil, a2a.ErrInvalidParams
	}
	if _, err := h.GetTask(ctx, &a2a.GetTaskRequest{ID: req.ID}); err != nil {
		return nil, err
	}
	var m Message
	if err := h.call(ctx, "ax.network.cancel", object{"message_id": req.ID}, &m); err != nil {
		return nil, a2a.ErrTaskNotCancelable
	}
	return networkTaskView(m, nil), nil
}
func (h *networkA2AHandler) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	if err := h.checkVersion(); err != nil {
		return nil, err
	}
	if req == nil || req.Tenant != "" || req.Message == nil || req.Message.Role != a2a.MessageRoleUser || !validID.MatchString(req.Message.ID) || len(req.Message.Parts) != 1 || req.Message.TaskID != "" || req.Message.ContextID != "" {
		return nil, a2a.ErrInvalidParams
	}
	if !slices.Contains(req.Message.Extensions, networkAXExtension) || !slices.ContainsFunc(strings.Split(h.extensions, ","), func(v string) bool { return strings.TrimSpace(v) == networkAXExtension }) {
		return nil, a2a.ErrExtensionSupportRequired
	}
	if req.Config != nil {
		if req.Config.PushConfig != nil {
			return nil, a2a.ErrPushNotificationNotSupported
		}
		if req.Config.HistoryLength != nil && (*req.Config.HistoryLength < 0 || *req.Config.HistoryLength > 20) {
			return nil, a2a.ErrInvalidParams
		}
		if len(req.Config.AcceptedOutputModes) > 0 && !slices.Contains(req.Config.AcceptedOutputModes, "text/plain") {
			return nil, a2a.ErrUnsupportedContentType
		}
	}
	part := req.Message.Parts[0]
	if part == nil {
		return nil, a2a.ErrInvalidParams
	}
	text, ok := part.Content.(a2a.Text)
	if !ok {
		return nil, a2a.ErrUnsupportedContentType
	}
	var envelope networkEnvelope
	metadata, exists := req.Message.Metadata[networkAXExtension]
	if !exists || json.Unmarshal(raw(metadata), &envelope) != nil || envelope.ID != req.Message.ID || envelope.Recipient != h.recipient || envelope.Text != "" {
		return nil, a2a.ErrInvalidParams
	}
	envelope.Text = string(text)
	var m Message
	if err := h.call(ctx, "ax.network.receive", object{"envelope": envelope}, &m); err != nil {
		return nil, err
	}
	var history *int
	if req.Config != nil {
		history = req.Config.HistoryLength
	}
	if req.Config != nil && req.Config.ReturnImmediately {
		return networkTaskView(m, history), nil
	}
	// Default A2A sends wait for an explicit outcome, never treating a receipt
	// or ordinary reply as success. This wait is bounded by the HTTP deadline.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state := networkTaskState(m)
		if state.Terminal() || state == a2a.TaskStateInputRequired || state == a2a.TaskStateAuthRequired {
			return networkTaskView(m, history), nil
		}
		select {
		case <-ctx.Done():
			return nil, a2a.NewError(a2a.ErrServerError, "Task committed; wait deadline reached. Use GetTask with its stable message ID or retry the same message ID.")
		case <-ticker.C:
			if err := h.call(ctx, "ax.network.task", object{"message_id": m.ID}, &m); err != nil {
				return nil, err
			}
		}
	}
}

var _ a2asrv.RequestHandler = (*networkA2AHandler)(nil)
