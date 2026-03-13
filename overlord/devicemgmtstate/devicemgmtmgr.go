// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2026 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// Package devicemgmtstate implements the manager and state aspects responsible
// for message-based remote device management. It receives signed request-message
// assertions from the store via periodic message exchanges, validates them against
// SD187 requirements, dispatches them to subsystem-specific handlers (like confdb),
// and sends back response-message assertions with processing results.
package devicemgmtstate

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/features"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/overlord/swfeats"
	"github.com/snapcore/snapd/store"
	"gopkg.in/tomb.v2"
)

const (
	deviceMgmtStateKey = "device-mgmt"

	defaultExchangeLimit    = 10
	defaultExchangeInterval = 6 * time.Hour

	maxSequences = 256
)

var (
	timeNow = time.Now

	deviceMgmtExchangeChangeKind = swfeats.RegisterChangeKind("device-management-exchange")
)

// MessageHandler processes request messages of a specific kind.
// Caller must hold state lock when using this interface.
type MessageHandler interface {
	// Validate checks subsystem-specific constraints (authorization, payload schema, etc).
	Validate(st *state.State, msg *RequestMessage) error

	// Apply processes a request-message and returns a change ID.
	Apply(st *state.State, reqAs *RequestMessage) (changeID string, err error)

	// BuildResponse converts a completed change into a response body and status.
	BuildResponse(chg *state.Change) (body map[string]any, status asserts.MessageStatus)
}

// ResponseMessageSigner can sign response-message assertions.
type ResponseMessageSigner interface {
	SignResponseMessage(accountID, messageID string, status asserts.MessageStatus, body []byte) (*asserts.ResponseMessage, error)
}

// RequestMessage represents a request-message being processed.
// Messages remain pending until their associated change completes,
// at which point a response is queued and the message is removed.
type RequestMessage struct {
	AccountID   string    `json:"account-id"`
	AuthorityID string    `json:"authority-id"`
	BaseID      string    `json:"base-id"`
	SeqNum      int       `json:"seq-num"`
	Kind        string    `json:"kind"`
	Devices     []string  `json:"devices"`
	ValidSince  time.Time `json:"valid-since"`
	ValidUntil  time.Time `json:"valid-until"`
	Body        string    `json:"body"`

	ReceiveTime time.Time             `json:"receive-time"`
	Status      asserts.MessageStatus `json:"status,omitempty"`
	Error       string                `json:"error,omitempty"`
	// Subsystem change applying this message.
	ChangeID string `json:"change-id,omitempty"`
}

// ID returns the full message identifier `BaseID[-SeqNum]`.
func (msg *RequestMessage) ID() string {
	if msg.SeqNum != 0 {
		return fmt.Sprintf("%s-%d", msg.BaseID, msg.SeqNum)
	}

	return msg.BaseID
}

// sequenceState holds the messages and progress for a single base ID,
// covering both sequenced & unsequenced messages.
type sequenceState struct {
	// Messages holds request messages from receipt until their response is queued.
	Messages []*RequestMessage `json:"messages"`

	// Applied is the highest sequence number successfully applied. A sequenced
	// message can only be applied once its predecessor has been applied.
	Applied int `json:"applied"`
}

// deviceMgmtState holds the persistent state for device management operations.
type deviceMgmtState struct {
	// Sequences maps base IDs to their per-base-ID state.
	Sequences map[string]*sequenceState `json:"sequences"`

	// SequenceLRU tracks sequenced base IDs in least-recently-used order for eviction.
	SequenceLRU []string `json:"sequence-lru"`

	// LastReceivedToken is the token of the last message successfully stored locally,
	// sent in the "after" field of the next exchange to acknowledge receipt
	// up to this point.
	LastReceivedToken string `json:"last-received-token"`

	// ReadyResponses are response messages ready to send in the next exchange.
	// Cleared after successful transmission.
	ReadyResponses map[string]store.Message `json:"ready-responses"`

	// LastExchangeTime is the timestamp of the last message exchange.
	LastExchangeTime time.Time `json:"last-exchange-time"`
}

// enqueueRequests queues incoming request messages for processing
// and updates polling state accordingly.
func (ms *deviceMgmtState) enqueueRequests(pollResp *store.MessageExchangeResponse) {
	for _, msg := range pollResp.Messages {
		reqMsg, err := parseRequestMessage(msg.Message)
		if err != nil {
			// Malformed messages are acknowledged but not processed.
			// There's no point retrying since if parsing fails once, it will fail again.
			logger.Noticef("cannot parse request-message with token %s: %v", msg.Token, err)
			continue
		}

		seq := ms.Sequences[reqMsg.BaseID]
		if seq == nil {
			seq = &sequenceState{}
			ms.Sequences[reqMsg.BaseID] = seq
		}

		found := false
		for _, existing := range seq.Messages {
			if existing.SeqNum == reqMsg.SeqNum {
				found = true
				break
			}
		}
		if !found {
			seq.Messages = append(seq.Messages, reqMsg)
		}

		if reqMsg.SeqNum > 0 {
			ms.touchSequence(reqMsg.BaseID)
		}
	}

	if len(pollResp.Messages) > 0 {
		token := pollResp.Messages[len(pollResp.Messages)-1].Token
		ms.LastReceivedToken = token
	} else {
		ms.LastReceivedToken = ""
	}

	ms.ReadyResponses = make(map[string]store.Message)
	ms.LastExchangeTime = timeNow()
}

// touchSequence marks a sequence as recently used, adding it if new.
func (ms *deviceMgmtState) touchSequence(baseID string) {
	// Move sequence to end (most recently used).
	for i, id := range ms.SequenceLRU {
		if id == baseID {
			ms.SequenceLRU = append(ms.SequenceLRU[:i], ms.SequenceLRU[i+1:]...)
			break
		}
	}

	ms.SequenceLRU = append(ms.SequenceLRU, baseID)
}

// evictLRUSequence evicts the least recently used sequence. It returns its earliest
// pending message for rejection; remaining messages in the sequence are deleted.
// The returned message is cleaned up by queue-mgmt-response after its response is queued.
func (ms *deviceMgmtState) evictLRUSequence() *RequestMessage {
	if len(ms.SequenceLRU) == 0 {
		return nil
	}

	baseID := ms.SequenceLRU[0]
	ms.SequenceLRU = ms.SequenceLRU[1:]

	seq := ms.Sequences[baseID]
	if seq == nil || len(seq.Messages) == 0 {
		delete(ms.Sequences, baseID)
		return nil
	}

	earliest := seq.Messages[0]
	for _, msg := range seq.Messages[1:] {
		if msg.SeqNum < earliest.SeqNum {
			earliest = msg
		}
	}

	seq.Messages = []*RequestMessage{earliest}
	return earliest
}

// DeviceMgmtManager handles device management operations.
type DeviceMgmtManager struct {
	state    *state.State
	signer   ResponseMessageSigner
	handlers map[string]MessageHandler
}

// Manager creates a new DeviceMgmtManager.
func Manager(state *state.State, runner *state.TaskRunner, signer ResponseMessageSigner) *DeviceMgmtManager {
	m := &DeviceMgmtManager{
		state:    state,
		signer:   signer,
		handlers: make(map[string]MessageHandler),
	}

	runner.AddHandler("exchange-mgmt-messages", m.doExchangeMessages, nil)
	runner.AddHandler("dispatch-mgmt-messages", m.doDispatchMessages, nil)
	runner.AddHandler("validate-mgmt-message", m.doValidateMessage, nil)
	runner.AddHandler("apply-mgmt-message", m.doApplyMessage, nil)
	runner.AddHandler("queue-mgmt-response", m.doQueueResponse, nil)

	return m
}

// getState retrieves the current device management state, initializing if not present.
func (m *DeviceMgmtManager) getState() (*deviceMgmtState, error) {
	var ms deviceMgmtState
	err := m.state.Get(deviceMgmtStateKey, &ms)
	if err != nil {
		if errors.Is(err, state.ErrNoState) {
			return &deviceMgmtState{
				Sequences:      make(map[string]*sequenceState),
				ReadyResponses: make(map[string]store.Message),
			}, nil
		}

		return nil, err
	}

	if ms.Sequences == nil {
		ms.Sequences = make(map[string]*sequenceState)
	}

	if ms.ReadyResponses == nil {
		ms.ReadyResponses = map[string]store.Message{}
	}

	return &ms, nil
}

// setState persists the device management state.
func (m *DeviceMgmtManager) setState(ms *deviceMgmtState) {
	m.state.Set(deviceMgmtStateKey, ms)
}

// Ensure implements StateManager.Ensure.
func (m *DeviceMgmtManager) Ensure() error {
	m.state.Lock()
	defer m.state.Unlock()

	ms, err := m.getState()
	if err != nil {
		return err
	}

	if !m.shouldExchangeMessages(ms) {
		return nil
	}

	// For now, only one device management change can be in flight at any given time.
	for _, chg := range m.state.Changes() {
		if chg.Kind() == deviceMgmtExchangeChangeKind && !chg.Status().Ready() {
			return nil
		}
	}

	chg := m.state.NewChange(deviceMgmtExchangeChangeKind, "Process device management messages")

	exchg := m.state.NewTask("exchange-mgmt-messages", "Exchange messages with the Store")
	chg.AddTask(exchg)

	dispatch := m.state.NewTask("dispatch-mgmt-messages", "Dispatch message(s) to subsystems")
	dispatch.WaitFor(exchg)
	chg.AddTask(dispatch)

	m.state.EnsureBefore(0)

	return nil
}

// isRemoteDeviceManagementEnabled checks whether the remote device management feature is enabled.
func (m *DeviceMgmtManager) isRemoteDeviceManagementEnabled() bool {
	tr := config.NewTransaction(m.state)
	enabled, err := features.Flag(tr, features.RemoteDeviceManagement)
	if err != nil && !config.IsNoOption(err) {
		logger.Noticef("cannot check remote-device-management feature flag: %v", err)

		// If the flag cannot be checked, assume disabled.
		return false
	}

	return enabled
}

// shouldExchangeMessages checks whether a message exchange should happen now.
func (m *DeviceMgmtManager) shouldExchangeMessages(ms *deviceMgmtState) bool {
	nextExchange := ms.LastExchangeTime.Add(defaultExchangeInterval)
	if timeNow().Before(nextExchange) {
		return false
	}

	// If disabled, still exchange to deliver responses for already-processed messages.
	return m.isRemoteDeviceManagementEnabled() || len(ms.ReadyResponses) > 0
}

// doExchangeMessages exchanges messages with the store: sends queued response messages,
// acknowledges receipt of persisted request messages, and fetches new request messages.
func (m *DeviceMgmtManager) doExchangeMessages(t *state.Task, tomb *tomb.Tomb) error {
	m.state.Lock()
	defer m.state.Unlock()

	ms, err := m.getState()
	if err != nil {
		return err
	}

	deviceCtx, err := snapstate.DevicePastSeeding(m.state, nil)
	if err != nil {
		return err
	}
	sto := snapstate.Store(m.state, deviceCtx)

	limit := 0
	if m.isRemoteDeviceManagementEnabled() {
		limit = defaultExchangeLimit
	}

	messages := make([]store.Message, 0, len(ms.ReadyResponses))
	for _, msg := range ms.ReadyResponses {
		messages = append(messages, msg)
	}

	m.state.Unlock()
	pollResp, err := sto.ExchangeMessages(tomb.Context(nil), &store.MessageExchangeRequest{
		After:    ms.LastReceivedToken,
		Limit:    limit,
		Messages: messages,
	})
	m.state.Lock()
	if err != nil {
		return err
	}

	ms.enqueueRequests(pollResp)
	m.setState(ms)

	return nil
}

// doDispatchMessages selects pending requests for processing and queues tasks for them.
func (m *DeviceMgmtManager) doDispatchMessages(t *state.Task, _ *tomb.Tomb) error {
	m.state.Lock()
	defer m.state.Unlock()

	ms, err := m.getState()
	if err != nil {
		return err
	}

	chg := t.Change()
	for len(ms.SequenceLRU) > maxSequences {
		earliest := ms.evictLRUSequence()
		if earliest != nil {
			earliest.Status = asserts.MessageStatusRejected
			earliest.Error = "cannot process message: sequence evicted from cache due to capacity limits"

			lane := m.state.NewLane()
			queue := m.state.NewTask("queue-mgmt-response", fmt.Sprintf("Queue response for message with id %q", earliest.ID()))
			queue.Set("id", earliest.ID())
			queue.JoinLane(lane)
			chg.AddTask(queue)
		}
	}

	for baseID, seq := range ms.Sequences {
		var undispatched []*RequestMessage
		for _, msg := range seq.Messages {
			if msg.ChangeID == "" && msg.Status == "" {
				undispatched = append(undispatched, msg)
			}
		}

		if len(undispatched) > 0 {
			m.dispatchSequence(ms, t, baseID, undispatched)
		}
	}

	m.setState(ms)

	return nil
}

// dispatchSequence dispatches sequenced messages starting from where the sequence left off,
// chaining consecutive messages. Gaps in the sequence stop the chain.
// All messages must belong to the same sequence.
func (m *DeviceMgmtManager) dispatchSequence(ms *deviceMgmtState, dispatchTask *state.Task, baseID string, msgs []*RequestMessage) {
	if len(msgs) == 0 {
		return
	}

	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].SeqNum < msgs[j].SeqNum
	})

	expectedSeqNum := ms.Sequences[baseID].Applied + 1
	if msgs[0].SeqNum == 0 {
		expectedSeqNum = 0
	}

	awaitTask := dispatchTask
	for _, msg := range msgs {
		if msg.SeqNum != expectedSeqNum {
			// Gap in sequence, stop chaining.
			break
		}

		awaitTask = m.dispatchMessage(awaitTask, msg)
		expectedSeqNum++
	}
}

// dispatchMessage creates the task chain for a single message and returns
// the final task so callers can chain subsequent messages after it.
func (m *DeviceMgmtManager) dispatchMessage(prevTask *state.Task, msg *RequestMessage) *state.Task {
	chg := prevTask.Change()
	lane := m.state.NewLane()

	addTask := func(kind, summary string) {
		t := m.state.NewTask(kind, summary)
		t.Set("id", msg.ID())
		t.WaitFor(prevTask)
		t.JoinLane(lane)
		chg.AddTask(t)

		prevTask = t
	}

	addTask("validate-mgmt-message", fmt.Sprintf("Validate message with id %q", msg.ID()))
	addTask("apply-mgmt-message", fmt.Sprintf("Apply message with id %q", msg.ID()))
	addTask("queue-mgmt-response", fmt.Sprintf("Queue response for message with id %q", msg.ID()))

	return prevTask
}

// doValidateMessage performs snapd-level and subsystem-level validation on a message.
func (m *DeviceMgmtManager) doValidateMessage(t *state.Task, _ *tomb.Tomb) error {
	// TODO: implement this task, no-op for now.
	return nil
}

// doApplyMessage dispatches the message to its subsystem handler for processing.
func (m *DeviceMgmtManager) doApplyMessage(t *state.Task, _ *tomb.Tomb) error {
	// TODO: implement this task, no-op for now.
	return nil
}

// doQueueResponse builds a response, signs it, and queues it for transmission on the next exchange.
// Retries until subsystem change completes.
func (m *DeviceMgmtManager) doQueueResponse(t *state.Task, _ *tomb.Tomb) error {
	// TODO: implement this task, no-op for now.
	return nil
}

// parseRequestMessage decodes a store message body into a RequestMessage.
func parseRequestMessage(msg store.Message) (*RequestMessage, error) {
	if msg.Format != "assertion" {
		return nil, fmt.Errorf("cannot process assertion: unsupported format %q", msg.Format)
	}

	as, err := asserts.Decode([]byte(msg.Data))
	if err != nil {
		return nil, fmt.Errorf("cannot decode assertion: %w", err)
	}

	reqAs, ok := as.(*asserts.RequestMessage)
	if !ok {
		return nil, fmt.Errorf(`cannot process assertion: expected "request-message" but got %q`, as.Type().Name)
	}

	devices := reqAs.Devices()
	deviceIDs := make([]string, len(devices))
	for i, devID := range devices {
		deviceIDs[i] = devID.String()
	}

	return &RequestMessage{
		AccountID:   reqAs.AccountID(),
		AuthorityID: reqAs.AuthorityID(),
		BaseID:      reqAs.ID(),
		SeqNum:      reqAs.SeqNum(),
		Kind:        reqAs.Kind(),
		Devices:     deviceIDs,
		ValidSince:  reqAs.ValidSince(),
		ValidUntil:  reqAs.ValidUntil(),
		Body:        string(reqAs.Body()),
		ReceiveTime: timeNow(),
	}, nil
}
