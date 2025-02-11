package confdbstate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/overlord/devicestate"
	"github.com/snapcore/snapd/overlord/state"
)

type Payload struct {
	Account string `json:"account,omitempty"`
	Confdb  string `json:"confdb,omitempty"`
	Service string `json:"service,omitempty"`
	Status  string `json:"status,omitempty"`
	Type    string `json:"type,omitempty"`
	Value   any    `json:"value,omitempty"`
	View    string `json:"view,omitempty"`
}

type Request struct {
	Status   string   `json:"status"`
	Messages []string `json:"value"`
}

type ConfdbControlManager struct {
	st     *state.State
	serial *asserts.Serial
	devMgr *devicestate.DeviceManager
}

func CCManager(st *state.State, runner *state.TaskRunner, devMgr *devicestate.DeviceManager) *ConfdbControlManager {
	st.Lock()
	serial, _ := devMgr.Serial()
	st.Unlock()
	return &ConfdbControlManager{st: st, serial: serial, devMgr: devMgr}
}

func (m *ConfdbControlManager) Ensure() error {
	logger.Notice("confdb-control: fetching messages...")
	msgs := m.getMessages()
	for _, msg := range msgs {
		m.handleMessage(msg)
	}
	return nil
}

func (m *ConfdbControlManager) getMessages() []*asserts.RequestMessage {
	url := fmt.Sprintf("http://localhost:5000/pull/devices/%s", m.serial.Serial())
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

	var result []*asserts.RequestMessage
	for _, message := range req.Messages {
		r := bytes.NewReader([]byte(message))
		dec := asserts.NewDecoder(r)
		a, err := dec.Decode()
		if err != nil {
			logger.Noticef("cannot decode assertion: %s", err)
			return nil
		}

		result = append(result, a.(*asserts.RequestMessage))
	}

	return result
}

func (m *ConfdbControlManager) handleMessage(msg *asserts.RequestMessage) {
	logger.Notice("processing message...")

	var payload Payload
	err := json.Unmarshal(msg.Body(), &payload)
	if err != nil {
		logger.Noticef("cannot unmarshal payload %s: %s", string(msg.Body()), err)
	}

	m.st.Lock()
	var value any
	switch payload.Type {
	case "get-confdb-value":
		value, err = Get(m.st, payload.Account, payload.Confdb, payload.View, nil)
	case "set-confdb-value":
		err = Set(m.st, payload.Account, payload.Confdb, payload.View, payload.Value.(map[string]any))
	default:
		err = fmt.Errorf("unknown message type: %s", payload.Type)
	}

	result := Payload{Type: "response"}
	if err != nil {
		result.Status = "err"
		result.Value = fmt.Errorf("an error occurred: %s", err)
	} else {
		result.Status = "ok"
		result.Value = value
	}

	dump, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		logger.Noticef("cannot marshal payload: %s", err)
	}

	resp, err := m.devMgr.SignResponseMessage("confdb-control", msg.HeaderString("message-id"), dump)
	if err != nil {
		logger.Noticef("cannot sign message: %s", err)
		return
	}
	defer m.st.Unlock()

	m.sendResponse(payload.Service, resp)
}

func (m *ConfdbControlManager) sendResponse(service string, msg *asserts.ResponseMessage) {
	logger.Notice("sending response...")

	url := fmt.Sprintf("http://localhost:5000/push/services/%s", service)
	body := asserts.Encode(msg)

	_, err := http.Post(url, "application/x.ubuntu.assertion", bytes.NewReader(body))
	if err != nil {
		logger.Noticef("cannot send message: %s", err)
	}
}
