//go:build darwin || linux

package ax

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
)

func networkWireFixture(t *testing.T) (string, string, networkIdentity, networkIdentity, tls.Certificate, tls.Certificate, *http.Client, *http.Client) {
	t.Helper()
	a, b := startTestServer(t), startTestServer(t)
	ia, ca := testNetworkIdentity(t, a, "laptop", "100.64.0.1")
	ib, cb := testNetworkIdentity(t, b, "buildbox", "100.64.0.2")
	for _, p := range []struct {
		dir, alias string
		card       networkCard
		exports    []string
	}{{a, "buildbox", ib.Card, []string{"web"}}, {b, "laptop", ia.Card, []string{"api"}}} {
		if err := trustNetworkPeer(p.dir, p.alias, p.card, p.exports); err != nil {
			t.Fatal(err)
		}
		if err := setNetworkMessaging(p.dir, p.alias, true); err != nil {
			t.Fatal(err)
		}
	}
	aa, stopA := testNetworkServer(t, a, ia, ca)
	ab, stopB := testNetworkServer(t, b, ib, cb)
	t.Cleanup(stopA)
	t.Cleanup(stopB)
	return a, b, ia, ib, ca, cb, testNetworkClient(t, ca, ib.Card, ab), testNetworkClient(t, cb, ia.Card, aa)
}

func TestNetworkA2AStandardClientAndTaskOutcomes(t *testing.T) {
	a, b, _, ib, _, _, httpClient, _ := networkWireFixture(t)
	web := startAttachedMCP(t, a, "-n", "web", "-s", "web_session", "-p", "default")
	api := startAttachedMCP(t, b, "-n", "api", "-s", "api_session", "-p", "default")
	var agents struct{ Agents []Agent }
	if err := api.call(t, "list_agents", object{}, &agents); err != "" {
		t.Fatal(err)
	}
	recipient := agents.Agents[0].ID
	response, err := httpClient.Get(ib.Card.Address + networkA2APath + recipient + "/agent-card.json")
	if err != nil {
		t.Fatal(err)
	}
	var card a2a.AgentCard
	err = json.NewDecoder(response.Body).Decode(&card)
	response.Body.Close()
	if err != nil || card.Capabilities.Streaming || card.Capabilities.PushNotifications || len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolVersion != a2a.Version {
		t.Fatalf("invalid capabilities: %+v %v", card, err)
	}
	client, err := a2aclient.NewFromCard(context.Background(), &card, a2aclient.WithJSONRPCTransport(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	// This independently constructed SDK client exercises the advertised card.
	envelope := networkEnvelope{ID: "wire_request", SenderID: "remote_web", SenderName: "web", SenderHost: "codex", Recipient: recipient, Text: "check the schema", Expires: time.Now().Add(time.Minute).UnixMilli()}
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(envelope.Text))
	msg.ID = envelope.ID
	msg.Extensions = []string{networkAXExtension}
	wire := envelope
	wire.Text = ""
	msg.Metadata = map[string]any{networkAXExtension: wire}
	// SDK Client injects its version header; extension activation is explicit.
	client, err = a2aclient.NewFromCard(context.Background(), &card, a2aclient.WithJSONRPCTransport(httpClient), a2aclient.WithCallInterceptors(networkTestExtension{}))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatal(err)
	}
	task := result.(*a2a.Task)
	if task.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("receipt implied completion: %+v", task)
	}
	retry, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil || retry.(*a2a.Task).ID != task.ID {
		t.Fatalf("duplicate changed task: %v", err)
	}
	page, err := client.ListTasks(context.Background(), &a2a.ListTasksRequest{})
	if err != nil || len(page.Tasks) != 1 || page.TotalSize != 1 {
		t.Fatalf("lost scoped task: %+v %v", page, err)
	}
	var incoming struct{ Message *Message }
	if err := api.call(t, "check_inbox", object{}, &incoming); err != "" || incoming.Message == nil {
		t.Fatalf("MCP receive: %s", err)
	}
	task, err = client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	if err != nil || task.Status.State != a2a.TaskStateWorking {
		t.Fatalf("handoff state: %+v %v", task, err)
	}
	if _, err = client.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID}); !errors.Is(err, a2a.ErrTaskNotCancelable) {
		t.Fatalf("claimed to cancel native work: %v", err)
	}
	if err := api.call(t, "ack_message", object{"message_id": task.ID}, nil); err != "" {
		t.Fatal(err)
	}
	task, err = client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	if err != nil || task.Status.State != a2a.TaskStateWorking {
		t.Fatal("acknowledgment implied task completion")
	}
	if err := api.call(t, "report_task", object{"message_id": task.ID, "state": "completed"}, nil); err != "" {
		t.Fatal(err)
	}
	task, err = client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID})
	if err != nil || task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("explicit result lost: %+v %v", task, err)
	}
	if err := web.call(t, "report_task", object{"message_id": task.ID, "state": "failed"}, nil); err == "" {
		t.Fatal("unrelated local session changed task")
	}
	if err := revokeNetworkPeer(b, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.GetTask(context.Background(), &a2a.GetTaskRequest{ID: task.ID}); err == nil {
		t.Fatal("revoked client read task")
	}
}

func TestNetworkWorkersMCPRoundTrip(t *testing.T) {
	a, b, ia, ib, ca, cb, clientA, clientB := networkWireFixture(t)
	web := startAttachedMCP(t, a, "-n", "web", "-s", "web_session", "-p", "default")
	api := startAttachedMCP(t, b, "-n", "api", "-s", "api_session", "-p", "default")
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(doneA)
		runNetworkWorkersWithClients(ctx, a, ca, func(tls.Certificate, networkCard) (*http.Client, error) { return clientA, nil })
	}()
	go func() {
		defer close(doneB)
		runNetworkWorkersWithClients(ctx, b, cb, func(tls.Certificate, networkCard) (*http.Client, error) { return clientB, nil })
	}()
	defer func() {
		cancel()
		for _, done := range []chan struct{}{doneA, doneB} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("worker did not stop")
			}
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var list struct{ Agents []Agent }
		if err := web.call(t, "list_agents", object{}, &list); err != "" {
			t.Fatal(err)
		}
		found := false
		for _, agent := range list.Agents {
			found = found || agent.Name == "api@buildbox"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("remote discovery did not populate MCP")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	var sent struct{ Message Message }
	if err := web.call(t, "send_message", object{"target": "api@buildbox", "text": "is the schema ready?", "client_message_id": "roundtrip"}, &sent); err != "" {
		t.Fatal(err)
	}
	receive := func(mcp *attachedMCP) Message {
		t.Helper()
		for {
			var got struct{ Message *Message }
			if err := mcp.call(t, "check_inbox", object{}, &got); err != "" {
				t.Fatal(err)
			}
			if got.Message != nil {
				return *got.Message
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("event-driven network delivery timed out")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	incoming := receive(api)
	if incoming.Sender.Name != "web@laptop" || incoming.Text != "is the schema ready?" {
		t.Fatalf("lost identity: %+v", incoming)
	}
	if err := api.call(t, "reply", object{"message_id": incoming.ID, "text": "yes, schema ready", "client_message_id": "answer"}, nil); err != "" {
		t.Fatal(err)
	}
	reply := receive(web)
	if reply.Parent != sent.Message.ID || reply.Text != "yes, schema ready" || reply.Sender.Name != "api@buildbox" {
		t.Fatalf("reply missed original session: %+v", reply)
	}
	t.Logf("MCP round trip over pinned TLS and official A2A SDK: %s (%s to %s)", time.Since(start), ia.Card.Name, ib.Card.Name)
}

type networkTestExtension struct {
	a2aclient.PassthroughInterceptor
}

func (networkTestExtension) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	req.ServiceParams.Append(a2a.SvcParamExtensions, networkAXExtension)
	return ctx, nil, nil
}

func TestNetworkA2AErrorsCancellationAndBoundedWait(t *testing.T) {
	_, b, _, ib, _, _, client, _ := networkWireFixture(t)
	api := startAttachedMCP(t, b, "-n", "api", "-s", "api_session", "-p", "default")
	var agents struct{ Agents []Agent }
	if err := api.call(t, "list_agents", object{}, &agents); err != "" {
		t.Fatal(err)
	}
	id := agents.Agents[0].ID
	transport := a2aclient.NewJSONRPCTransport(ib.Card.Address+networkA2APath+id, client)
	params := a2aclient.ServiceParams{a2a.SvcParamVersion: []string{string(a2a.Version)}, a2a.SvcParamExtensions: []string{networkAXExtension}}
	env := networkEnvelope{ID: "cancel_request", SenderID: "remote_web", SenderName: "web", SenderHost: "codex", Recipient: id, Text: "queued task", Expires: time.Now().Add(time.Minute).UnixMilli()}
	send := func(e networkEnvelope) *a2a.SendMessageRequest {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(e.Text))
		m.ID = e.ID
		m.Extensions = []string{networkAXExtension}
		e.Text = ""
		m.Metadata = map[string]any{networkAXExtension: e}
		return &a2a.SendMessageRequest{Message: m, Config: &a2a.SendMessageConfig{ReturnImmediately: true}}
	}
	wrong := a2aclient.ServiceParams{a2a.SvcParamVersion: []string{"0.3"}}
	if _, err := transport.SendMessage(context.Background(), wrong, send(env)); !errors.Is(err, a2a.ErrVersionNotSupported) {
		t.Fatalf("wrong version error: %v", err)
	}
	if _, err := transport.SendMessage(context.Background(), nil, send(env)); !errors.Is(err, a2a.ErrVersionNotSupported) {
		t.Fatalf("missing version error: %v", err)
	}
	missing := send(env)
	missing.Message.Extensions = nil
	if _, err := transport.SendMessage(context.Background(), params, missing); !errors.Is(err, a2a.ErrExtensionSupportRequired) {
		t.Fatalf("missing extension error: %v", err)
	}
	result, err := transport.SendMessage(context.Background(), params, send(env))
	if err != nil {
		t.Fatal(err)
	}
	task := result.(*a2a.Task)
	env.Text = "different task"
	if _, err := transport.SendMessage(context.Background(), params, send(env)); err == nil {
		t.Fatal("accepted contradictory retry")
	}
	for range 2 {
		cancelled, err := transport.CancelTask(context.Background(), params, &a2a.CancelTaskRequest{ID: task.ID})
		if err != nil || cancelled.Status.State != a2a.TaskStateCanceled {
			t.Fatalf("cancel retry: %+v %v", cancelled, err)
		}
	}
	var incoming struct{ Message *Message }
	if err := api.call(t, "check_inbox", object{}, &incoming); err != "" || incoming.Message != nil {
		t.Fatalf("cancelled work reached session: %s", err)
	}
	if _, err := transport.GetTaskPushConfig(context.Background(), params, &a2a.GetTaskPushConfigRequest{TaskID: task.ID, ID: "push"}); !errors.Is(err, a2a.ErrPushNotificationNotSupported) {
		t.Fatalf("push capability error: %v", err)
	}
	env.ID = "wait_request"
	env.Text = "await explicit completion"
	req := send(env)
	req.Config = nil
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := transport.SendMessage(ctx, params, req); err == nil {
		t.Fatal("default send returned before an explicit outcome")
	}
	page, err := transport.ListTasks(context.Background(), params, &a2a.ListTasksRequest{PageSize: 1})
	if err != nil || page.TotalSize != 2 || len(page.Tasks) != 1 || page.NextPageToken == "" {
		t.Fatalf("pagination: %+v %v", page, err)
	}
	next, err := transport.ListTasks(context.Background(), params, &a2a.ListTasksRequest{PageSize: 1, PageToken: page.NextPageToken})
	if err != nil || len(next.Tasks) != 1 || next.Tasks[0].Status.State != a2a.TaskStateSubmitted || next.NextPageToken != "" {
		t.Fatalf("committed blocking request disappeared: %+v %v", next, err)
	}
}

func TestNetworkA2AMaximumEscapedText(t *testing.T) {
	_, b, _, ib, _, _, client, _ := networkWireFixture(t)
	api := startAttachedMCP(t, b, "-n", "api", "-s", "escaped_session", "-p", "default")
	var agents struct{ Agents []Agent }
	if err := api.call(t, "list_agents", object{}, &agents); err != "" {
		t.Fatal(err)
	}
	text := strings.Repeat("\x01", maxText)
	envelope := networkEnvelope{ID: "escaped_request", SenderID: "remote_web", SenderName: "web", SenderHost: "codex", Recipient: agents.Agents[0].ID, Text: text, Expires: time.Now().Add(time.Minute).UnixMilli()}
	id, err := networkSend(context.Background(), client, ib.Card, &envelope)
	if err != nil {
		t.Fatal(err)
	}
	var incoming struct{ Message *Message }
	if err := api.call(t, "check_inbox", object{}, &incoming); err != "" || incoming.Message == nil || incoming.Message.ID != id || incoming.Message.Text != text {
		t.Fatal("legal escaped payload did not reach MCP unchanged")
	}
}

func TestNetworkWorkerRejectedRecipientDoesNotBlockOthers(t *testing.T) {
	a, b, _, _, _, _, client, _ := networkWireFixture(t)
	web := startAttachedMCP(t, a, "-n", "web", "-s", "web_session", "-p", "default")
	api := startAttachedMCP(t, b, "-n", "api", "-s", "api_session", "-p", "default")
	another := startAttachedMCP(t, b, "-n", "another", "-s", "other_session", "-p", "default")
	var list struct{ Agents []Agent }
	if err := api.call(t, "list_agents", object{}, &list); err != "" {
		t.Fatal(err)
	}
	admin, err := dial(socketPath(a))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.close()
	var snapshot []networkAgent
	for _, agent := range list.Agents {
		snapshot = append(snapshot, networkAgent{ID: agent.ID, Name: agent.Name, Host: agent.Host, State: agent.State})
	}
	if err = admin.call("ax.network.sync", object{"alias": "buildbox", "agents": snapshot}, nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"api@buildbox", "another@buildbox"} {
		if err := web.call(t, "send_message", object{"target": target, "text": "work", "client_message_id": "scope_" + strings.Split(target, "@")[0]}, nil); err != "" {
			t.Fatal(err)
		}
	}
	// The receiver changes its scope after discovery. The old target is denied;
	// the other target must still drain, without discarding the held request.
	peers, err := networkPeers(b)
	if err != nil {
		t.Fatal(err)
	}
	if err = trustNetworkPeer(b, "laptop", peers["laptop"].Card, []string{"another"}); err != nil {
		t.Fatal(err)
	}
	paired, err := networkPeers(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	go func() { defer close(done); networkPeerWorker(ctx, a, "buildbox", paired["buildbox"], client, wake) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker did not exit")
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var incoming struct{ Message *Message }
		if err := another.call(t, "check_inbox", object{}, &incoming); err != "" {
			t.Fatal(err)
		}
		if incoming.Message != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rejected recipient starved unrelated work")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var held *networkEnvelope
	if err = admin.call("ax.network.outbox", object{"alias": "buildbox"}, &held); err != nil || held == nil || held.Recipient == "" {
		t.Fatal("scope rejection discarded held task")
	}
}
