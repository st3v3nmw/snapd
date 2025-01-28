package confdbstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/state"
)

type Message struct {
	Type    string `json:"type"`
	Service string `json:"service"`
	Account string `json:"account"`
	Confdb  string `json:"confdb"`
	View    string `json:"view"`
	Value   any    `json:"value"`
}

type Request struct {
	Status   string     `json:"status"`
	Messages []*Message `json:"value"`
}

type Response struct {
	Message
	Status string `json:"status"`
}

type ConfdbControlManager struct {
	st       *state.State
	lastSync time.Time
}

func CCManager(st *state.State, runner *state.TaskRunner) *ConfdbControlManager {
	return &ConfdbControlManager{st: st}
}

func (m *ConfdbControlManager) Ensure() error {
	if time.Since(m.lastSync) < time.Duration(5*time.Minute) {
		return nil
	}

	logger.Noticef("confdb-control: Fetching messages...")
	msgs := m.getMessages()
	for _, msg := range msgs {
		m.handleMessage(msg)
	}

	m.lastSync = time.Now()
	return nil
}

func (m *ConfdbControlManager) getMessages() []*Message {
	url := "http://localhost:5000/pull/devices/some-device-id"
	res, err := http.Get(url)
	if err != nil {
		logger.Noticef("cannot call url: %s", err)
		return nil
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		logger.Noticef("cannot read response body: %s", err)
		return nil
	}

	var req Request
	err = json.Unmarshal(body, &req)
	if err != nil {
		logger.Noticef("cannot unmarshal json %s: %s", string(body), err)
		return nil
	}

	return req.Messages
}

func (m *ConfdbControlManager) handleMessage(msg *Message) {
	logger.Noticef("processing %v", msg)

	m.st.Lock()
	var err error
	var value any
	switch msg.Type {
	case "get-confdb-value":
		value, err = Get(m.st, msg.Account, msg.Confdb, msg.View, nil)
	case "set-confdb-value":
		err = Set(m.st, msg.Account, msg.Confdb, msg.View, msg.Value.(map[string]any))
	default:
		err = fmt.Errorf("unknown message type: %s", msg.Type)
	}
	m.st.Unlock()

	resp := &Response{
		Message: Message{
			Type:    "response",
			Service: msg.Service,
			Account: msg.Account,
			Confdb:  msg.Confdb,
			View:    msg.View,
		},
	}
	if err != nil {
		resp.Status = "err"
		resp.Value = fmt.Errorf("an error occurred: %s", err)
	} else {
		resp.Status = "ok"
		resp.Value = value
	}

	m.sendResponse(resp)
}

func (m *ConfdbControlManager) sendResponse(msg *Response) {
	logger.Noticef("sending %v", msg)

	url := fmt.Sprintf("http://localhost:5000/push/services/%s", msg.Service)
	body, err := json.Marshal(msg)
	if err != nil {
		logger.Noticef("cannot convert message to json: %s", err)
		return
	}

	_, err = http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		logger.Noticef("cannot send message: %s", err)
	}
}
