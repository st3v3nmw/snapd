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
 * along with this program. If not, see <http://www.gnu.org/licenses/>.
 *
 */

package confdbstate_test

import (
	"context"
	"fmt"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/confdb"
	"github.com/snapcore/snapd/overlord/confdbstate"
	"github.com/snapcore/snapd/overlord/devicemgmtstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/testutil"
)

type confdbHandlerSuite struct {
	testutil.BaseTest

	st     *state.State
	schema *confdb.Schema
}

var _ = Suite(&confdbHandlerSuite{})

func (s *confdbHandlerSuite) SetUpTest(c *C) {
	s.BaseTest.SetUpTest(c)

	s.st = state.New(nil)

	views := map[string]any{
		"wifi-admin": map[string]any{
			"rules": []any{
				map[string]any{"request": "ssid", "storage": "v1.wifi.ssid"},
				map[string]any{"request": "password", "storage": "v1.wifi.password"},
			},
		},
	}

	var err error
	s.schema, err = confdb.NewSchema("system", "network", views, confdb.NewJSONSchema())
	c.Assert(err, IsNil)
}

func (s *confdbHandlerSuite) TestValidateOK(c *C) {
	msg := &devicemgmtstate.RequestMessage{
		Kind: "confdb",
		Body: `{"action":"get","account":"system","view":"network/wifi-admin"}`,
	}
	handler := &confdbstate.ConfdbMessageHandler{}

	// Currently Validate is a no-op, just verify it doesn't error
	err := handler.Validate(s.st, msg)
	c.Assert(err, IsNil)
}

func (s *confdbHandlerSuite) TestApplyGetOK(c *C) {
	msg := &devicemgmtstate.RequestMessage{
		Kind: "confdb",
		Body: `{"action":"get","account":"system","view":"network/wifi-admin","keys":["ssid"]}`,
	}
	handler := &confdbstate.ConfdbMessageHandler{}

	restore := confdbstate.MockConfdbstateGetView(func(_ *state.State, account, schemaName, viewName string) (*confdb.View, error) {
		c.Check(account, Equals, "system")
		c.Check(schemaName, Equals, "network")
		c.Check(viewName, Equals, "wifi-admin")

		return s.schema.View(viewName), nil
	})
	defer restore()

	restore = confdbstate.MockConfdbstateReadConfdb(func(_ context.Context, _ *state.State, view *confdb.View, requests []string, _ map[string]any, _ confdb.Access) (string, error) {
		c.Check(view.Name, Equals, "wifi-admin")
		c.Check(requests, DeepEquals, []string{"ssid"})

		return "16384", nil
	})
	defer restore()

	chgID, err := handler.Apply(s.st, msg)
	c.Assert(err, IsNil)
	c.Check(chgID, Equals, "16384")
}

func (s *confdbHandlerSuite) TestApplySetOK(c *C) {
	msg := &devicemgmtstate.RequestMessage{
		Kind: "confdb",
		Body: `{"action":"set","account":"system","view":"network/wifi-admin","values":{"ssid":"my-network"}}`,
	}
	handler := &confdbstate.ConfdbMessageHandler{}

	restore := confdbstate.MockConfdbstateGetView(func(_ *state.State, _, _, viewName string) (*confdb.View, error) {
		return s.schema.View(viewName), nil
	})
	defer restore()

	restore = confdbstate.MockConfdbstateWriteConfdb(func(_ context.Context, _ *state.State, view *confdb.View, values map[string]any) (string, error) {
		c.Check(view.Name, Equals, "wifi-admin")
		c.Check(values, DeepEquals, map[string]any{"ssid": "my-network"})

		return "16384", nil
	})
	defer restore()

	chgID, err := handler.Apply(s.st, msg)
	c.Assert(err, IsNil)
	c.Check(chgID, Equals, "16384")
}

func (s *confdbHandlerSuite) TestApplyInvalidPayload(c *C) {
	type test struct {
		name        string
		body        string
		expectedErr string
	}

	tests := []test{
		{
			name:        "invalid json",
			body:        `{not valid json...`,
			expectedErr: "cannot decode confdb message body: .*",
		},
		{
			name:        "invalid view path",
			body:        `{"action":"get","account":"system","view":"network"}`,
			expectedErr: "invalid view path: expected 2 parts separated by /, got 1: network",
		},
	}

	handler := &confdbstate.ConfdbMessageHandler{}

	for _, tt := range tests {
		cmt := Commentf("%s test", tt.name)

		msg := &devicemgmtstate.RequestMessage{Kind: "confdb", Body: tt.body}

		chgID, err := handler.Apply(s.st, msg)
		c.Assert(err, NotNil, cmt)
		c.Check(err, ErrorMatches, tt.expectedErr, cmt)
		c.Check(chgID, Equals, "", cmt)
	}
}

func (s *confdbHandlerSuite) TestApplyGetViewError(c *C) {
	msg := &devicemgmtstate.RequestMessage{
		Kind: "confdb",
		Body: `{"action":"get","account":"system","view":"network/wifi-who"}`,
	}
	handler := &confdbstate.ConfdbMessageHandler{}

	restore := confdbstate.MockConfdbstateGetView(func(_ *state.State, _, _, _ string) (*confdb.View, error) {
		return nil, &confdbstate.NoViewError{}
	})
	defer restore()

	chgID, err := handler.Apply(s.st, msg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, "cannot find view .* in confdb schema .*")
	c.Check(chgID, Equals, "")
}

func (s *confdbHandlerSuite) TestApplyWriteConfdbError(c *C) {
	msg := &devicemgmtstate.RequestMessage{
		Kind: "confdb",
		Body: `{"action":"set","account":"system","view":"network/wifi-admin"}`,
	}
	handler := &confdbstate.ConfdbMessageHandler{}

	restore := confdbstate.MockConfdbstateGetView(func(_ *state.State, _, _, viewName string) (*confdb.View, error) {
		return s.schema.View(viewName), nil
	})
	defer restore()

	restore = confdbstate.MockConfdbstateWriteConfdb(func(_ context.Context, _ *state.State, _ *confdb.View, _ map[string]any) (string, error) {
		return "", fmt.Errorf("cannot write confdb")
	})
	defer restore()

	chgID, err := handler.Apply(s.st, msg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, "cannot write confdb")
	c.Check(chgID, Equals, "")
}

func (s *confdbHandlerSuite) TestApplyUnknownAction(c *C) {
	msg := &devicemgmtstate.RequestMessage{
		Kind: "confdb",
		Body: `{"action":"delete","account":"system","view":"network/wifi-admin"}`,
	}
	handler := &confdbstate.ConfdbMessageHandler{}

	restore := confdbstate.MockConfdbstateGetView(func(_ *state.State, _, _, viewName string) (*confdb.View, error) {
		return s.schema.View(viewName), nil
	})
	defer restore()

	chgID, err := handler.Apply(s.st, msg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, `cannot apply confdb message: unknown action "delete"`)
	c.Check(chgID, Equals, "")
}

func (s *confdbHandlerSuite) TestBuildResponseSuccessWithApiData(c *C) {
	s.st.Lock()
	defer s.st.Unlock()

	chg := s.st.NewChange("get-confdb", "test change")
	chg.SetStatus(state.DoneStatus)

	apiData := map[string]any{"values": map[string]any{"ssid": "my-network"}}
	chg.Set("api-data", apiData)

	handler := &confdbstate.ConfdbMessageHandler{}

	body, err := handler.ResultFromChange(chg)
	c.Assert(err, IsNil)
	c.Check(body, DeepEquals, apiData)
}

func (s *confdbHandlerSuite) TestBuildResponseSuccessNoApiData(c *C) {
	s.st.Lock()
	defer s.st.Unlock()

	chg := s.st.NewChange("get-confdb", "test change")
	chg.SetStatus(state.DoneStatus)

	handler := &confdbstate.ConfdbMessageHandler{}

	body, err := handler.ResultFromChange(chg)
	c.Assert(err, IsNil)
	c.Check(body, DeepEquals, map[string]any{})
}

func (s *confdbHandlerSuite) TestBuildResponseErrorInChange(c *C) {
	s.st.Lock()
	defer s.st.Unlock()

	chg := s.st.NewChange("get-confdb", "test change")
	chg.SetStatus(state.ErrorStatus)

	handler := &confdbstate.ConfdbMessageHandler{}

	body, err := handler.ResultFromChange(chg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, `internal inconsistency: change "get-confdb" in ErrorStatus with no task errors logged`)
	c.Check(body, IsNil)
}

func (s *confdbHandlerSuite) TestBuildResponseErrorInApiData(c *C) {
	s.st.Lock()
	defer s.st.Unlock()

	chg := s.st.NewChange("get-confdb", "test change")
	chg.SetStatus(state.DoneStatus)

	errBody := map[string]any{
		"kind":    "option-not-found",
		"message": "not found",
	}
	chg.Set("api-data", map[string]any{"error": errBody})

	handler := &confdbstate.ConfdbMessageHandler{}

	body, err := handler.ResultFromChange(chg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, "not found")
	c.Check(body, IsNil)
}

func (s *confdbHandlerSuite) TestBuildResponseApiDataGetError(c *C) {
	s.st.Lock()
	defer s.st.Unlock()

	chg := s.st.NewChange("get-confdb", "test change")
	chg.SetStatus(state.DoneStatus)

	chg.Set("api-data", "not a map")

	handler := &confdbstate.ConfdbMessageHandler{}

	body, err := handler.ResultFromChange(chg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, ".*cannot unmarshal.*")
	c.Check(body, IsNil)
}

func (s *confdbHandlerSuite) TestBuildResponseErrorMangledApiData(c *C) {
	s.st.Lock()
	defer s.st.Unlock()

	chg := s.st.NewChange("get-confdb", "test change")
	chg.SetStatus(state.DoneStatus)

	chg.Set("api-data", map[string]any{"error": "huh?"})

	handler := &confdbstate.ConfdbMessageHandler{}

	body, err := handler.ResultFromChange(chg)
	c.Assert(err, NotNil)
	c.Check(err, ErrorMatches, "no error context available")
	c.Check(body, IsNil)
}
