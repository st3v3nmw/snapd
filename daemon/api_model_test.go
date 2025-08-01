// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019-2021 Canonical Ltd
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

package daemon_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/check.v1"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/assertstest"
	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/client/clientutil"
	"github.com/snapcore/snapd/daemon"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/overlord/assertstate/assertstatetest"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/devicestate"
	"github.com/snapcore/snapd/overlord/devicestate/devicestatetest"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaptest"
)

var modelDefaults = map[string]any{
	"architecture": "amd64",
	"gadget":       "gadget",
	"kernel":       "kernel",
}

var _ = check.Suite(&modelSuite{})

type modelSuite struct {
	apiBaseSuite
}

func (s *modelSuite) TestPostRemodelUnhappy(c *check.C) {
	s.daemon(c)

	s.expectRootAccess()

	data, err := json.Marshal(daemon.PostModelData{NewModel: "invalid model"})
	c.Check(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/model", bytes.NewBuffer(data))
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Assert(rspe.Status, check.Equals, 400)
	c.Check(rspe.Message, check.Matches, "cannot decode new model assertion: .*")
}

func (s *modelSuite) TestPostRemodelUnhappyWrongAssertion(c *check.C) {
	s.daemon(c)

	s.expectRootAccess()

	acct := assertstest.NewAccount(s.StoreSigning, "developer1", nil, "")
	buf := bytes.NewBuffer(asserts.Encode(acct))

	data, err := json.Marshal(daemon.PostModelData{NewModel: buf.String()})
	c.Check(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/model", bytes.NewBuffer(data))
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Assert(rspe.Status, check.Equals, 400)
	c.Check(rspe.Message, check.Matches, "new model is not a model assertion: .*")
}

func (s *modelSuite) TestPostRemodel(c *check.C) {
	const offline = false
	s.testPostRemodel(c, offline)
}

func (s *modelSuite) TestPostRemodelOffline(c *check.C) {
	const offline = true
	s.testPostRemodel(c, offline)
}

func (s *modelSuite) testPostRemodel(c *check.C, offline bool) {
	s.expectRootAccess()

	oldModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)
	newModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults, map[string]any{
		"revision": "2",
	})

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)
	st := d.Overlord().State()
	st.Lock()
	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, oldModel)
	st.Unlock()

	soon := 0
	var origEnsureStateSoon func(*state.State)
	origEnsureStateSoon, restore := daemon.MockEnsureStateSoon(func(st *state.State) {
		soon++
		origEnsureStateSoon(st)
	})
	defer restore()

	var devicestateRemodelGotModel *asserts.Model
	defer daemon.MockDevicestateRemodel(func(st *state.State, nm *asserts.Model, opts devicestate.RemodelOptions) (*state.Change, error) {
		c.Check(opts.Offline, check.Equals, offline)
		devicestateRemodelGotModel = nm
		chg := st.NewChange("remodel", "...")
		return chg, nil
	})()

	// create a valid model assertion
	c.Assert(err, check.IsNil)
	modelEncoded := string(asserts.Encode(newModel))
	data, err := json.Marshal(daemon.PostModelData{
		NewModel: modelEncoded,
		Offline:  offline,
	})
	c.Check(err, check.IsNil)

	// set it and validate that this is what we was passed to
	// devicestateRemodel
	req, err := http.NewRequest("POST", "/v2/model", bytes.NewBuffer(data))
	c.Assert(err, check.IsNil)
	rsp := s.asyncReq(c, req, nil, actionIsExpected)
	c.Assert(rsp.Status, check.Equals, 202)
	c.Check(devicestateRemodelGotModel, check.DeepEquals, newModel)

	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)

	c.Assert(st.Changes(), check.HasLen, 1)
	chg1 := st.Changes()[0]
	c.Assert(chg, check.DeepEquals, chg1)
	c.Assert(chg.Kind(), check.Equals, "remodel")
	c.Assert(chg.Err(), check.IsNil)

	c.Assert(soon, check.Equals, 1)
}

func (s *modelSuite) TestPostRemodelWrongBody(c *check.C) {
	s.expectRootAccess()

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)

	type badBody struct {
		body, err string
	}
	for _, tc := range []badBody{
		{"", "cannot decode request body into remodel operation: EOF"},
		{"garbage", `cannot decode request body into remodel operation: invalid character 'g' looking for beginning of value`},
		{`{ "new-model": "garbage"}`, "cannot decode new model assertion: assertion content/signature separator not found"},
	} {
		req, err := http.NewRequest("POST", "/v2/model", bytes.NewBuffer([]byte(tc.body)))
		req.Header.Set("Content-Type", "application/json")
		c.Assert(err, check.IsNil)

		rspe := s.errorReq(c, req, nil, actionIsExpected)
		c.Assert(rspe.Status, check.Equals, 400)
		c.Assert(rspe.Kind, check.Equals, client.ErrorKind(""))
		c.Assert(rspe.Value, check.IsNil)
		c.Assert(rspe.Message, check.Equals, tc.err)
	}
}

func (s *modelSuite) TestPostRemodelWrongContentType(c *check.C) {
	s.expectRootAccess()

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)

	req, err := http.NewRequest("POST", "/v2/model", bytes.NewBuffer([]byte("garbage")))
	req.Header.Set("Content-Type", "footype")
	c.Assert(err, check.IsNil)

	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Assert(rspe.Status, check.Equals, 400)
	c.Assert(rspe.Kind, check.Equals, client.ErrorKind(""))
	c.Assert(rspe.Value, check.IsNil)
	c.Assert(rspe.Message, check.Equals, `unexpected media type "footype"`)

	req, err = http.NewRequest("POST", "/v2/model", bytes.NewBuffer([]byte("garbage")))
	req.Header.Set("Content-Type", "multipart/form-data")
	c.Assert(err, check.IsNil)

	rspe = s.errorReq(c, req, nil, actionIsExpected)
	c.Assert(rspe.Status, check.Equals, 400)
	c.Assert(rspe.Kind, check.Equals, client.ErrorKind(""))
	c.Assert(rspe.Value, check.IsNil)
	c.Assert(rspe.Message, check.Equals, `cannot read POST form: multipart: boundary is empty`)
}

func (s *modelSuite) TestGetModelNoModelAssertion(c *check.C) {

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)

	req, err := http.NewRequest("GET", "/v2/model", nil)
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Assert(rspe.Status, check.Equals, 404)
	c.Assert(rspe.Kind, check.Equals, client.ErrorKindAssertionNotFound)
	c.Assert(rspe.Value, check.Equals, "model")
	c.Assert(rspe.Message, check.Equals, "no model assertion yet")
}

func (s *modelSuite) TestGetModelHasModelAssertion(c *check.C) {
	// make a model assertion
	theModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)

	// model assertion setup
	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)
	st := d.Overlord().State()
	st.Lock()
	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, theModel)
	st.Unlock()

	// make a new get request to the model endpoint
	req, err := http.NewRequest("GET", "/v2/model", nil)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	s.req(c, req, nil, actionIsExpected).ServeHTTP(rec, req)

	// check that we get an assertion response
	c.Check(rec.Code, check.Equals, 200, check.Commentf("body %q", rec.Body))
	c.Check(rec.Header().Get("Content-Type"), check.Equals, "application/x.ubuntu.assertion")

	// check that there is only one assertion
	dec := asserts.NewDecoder(rec.Body)
	m, err := dec.Decode()
	c.Assert(err, check.IsNil)
	_, err = dec.Decode()
	c.Assert(err, check.Equals, io.EOF)

	// check that one of the assertion keys matches what's in the model we
	// provided
	c.Check(m.Type(), check.Equals, asserts.ModelType)
	arch := m.Header("architecture")
	c.Assert(arch, check.FitsTypeOf, "")
	c.Assert(arch.(string), check.Equals, "amd64")
}

func (s *modelSuite) TestGetModelJSONHasModelAssertion(c *check.C) {
	// make a model assertion
	theModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)

	// model assertion setup
	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)
	st := d.Overlord().State()
	st.Lock()
	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, theModel)
	st.Unlock()

	// make a new get request to the model endpoint with json as true
	req, err := http.NewRequest("GET", "/v2/model?json=true", nil)
	c.Assert(err, check.IsNil)
	rsp := s.syncReq(c, req, nil, actionIsExpected)
	// get the body and try to unmarshal into modelAssertJSON
	c.Assert(rsp.Result, check.FitsTypeOf, clientutil.ModelAssertJSON{})

	jsonResponse := rsp.Result.(clientutil.ModelAssertJSON)

	// get the architecture key from the headers
	arch, ok := jsonResponse.Headers["architecture"]
	c.Assert(ok, check.Equals, true)

	// ensure that the architecture key is what we set in the model defaults
	c.Assert(arch, check.FitsTypeOf, "")
	c.Assert(arch.(string), check.Equals, "amd64")
}

func (s *modelSuite) TestGetModelNoSerialAssertion(c *check.C) {

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)

	req, err := http.NewRequest("GET", "/v2/model/serial", nil)
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Assert(rspe.Status, check.Equals, 404)
	c.Assert(rspe.Kind, check.Equals, client.ErrorKindAssertionNotFound)
	c.Assert(rspe.Value, check.Equals, "serial")
	c.Assert(rspe.Message, check.Equals, "no serial assertion yet")
}

func (s *modelSuite) TestGetModelHasSerialAssertion(c *check.C) {
	// make a model assertion
	theModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)

	deviceKey, _ := assertstest.GenerateKey(752)

	encDevKey, err := asserts.EncodePublicKey(deviceKey.PublicKey())
	c.Assert(err, check.IsNil)

	// model assertion setup
	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)
	st := d.Overlord().State()
	st.Lock()
	defer st.Unlock()
	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, theModel)

	serial, err := s.Brands.Signing("my-brand").Sign(asserts.SerialType, map[string]any{
		"authority-id":        "my-brand",
		"brand-id":            "my-brand",
		"model":               "my-old-model",
		"serial":              "serialserial",
		"device-key":          string(encDevKey),
		"device-key-sha3-384": deviceKey.PublicKey().ID(),
		"timestamp":           time.Now().Format(time.RFC3339),
	}, nil, "")
	c.Assert(err, check.IsNil)
	assertstatetest.AddMany(st, serial)
	devicestatetest.SetDevice(st, &auth.DeviceState{
		Brand:  "my-brand",
		Model:  "my-old-model",
		Serial: "serialserial",
	})

	st.Unlock()
	defer st.Lock()

	// make a new get request to the serial endpoint
	req, err := http.NewRequest("GET", "/v2/model/serial", nil)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	s.req(c, req, nil, actionIsExpected).ServeHTTP(rec, req)

	// check that we get an assertion response
	c.Check(rec.Code, check.Equals, 200, check.Commentf("body %q", rec.Body))
	c.Check(rec.Header().Get("Content-Type"), check.Equals, "application/x.ubuntu.assertion")

	// check that there is only one assertion
	dec := asserts.NewDecoder(rec.Body)
	ser, err := dec.Decode()
	c.Assert(err, check.IsNil)
	_, err = dec.Decode()
	c.Assert(err, check.Equals, io.EOF)

	// check that the device key in the returned assertion matches what we
	// created above
	c.Check(ser.Type(), check.Equals, asserts.SerialType)
	devKey := ser.Header("device-key")
	c.Assert(devKey, check.FitsTypeOf, "")
	c.Assert(devKey.(string), check.Equals, string(encDevKey))
}

func (s *modelSuite) TestGetModelJSONHasSerialAssertion(c *check.C) {
	// make a model assertion
	theModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)

	deviceKey, _ := assertstest.GenerateKey(752)

	encDevKey, err := asserts.EncodePublicKey(deviceKey.PublicKey())
	c.Assert(err, check.IsNil)

	// model assertion setup
	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)
	st := d.Overlord().State()
	st.Lock()
	defer st.Unlock()
	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, theModel)

	serial, err := s.Brands.Signing("my-brand").Sign(asserts.SerialType, map[string]any{
		"authority-id":        "my-brand",
		"brand-id":            "my-brand",
		"model":               "my-old-model",
		"serial":              "serialserial",
		"device-key":          string(encDevKey),
		"device-key-sha3-384": deviceKey.PublicKey().ID(),
		"timestamp":           time.Now().Format(time.RFC3339),
	}, nil, "")
	c.Assert(err, check.IsNil)
	assertstatetest.AddMany(st, serial)
	devicestatetest.SetDevice(st, &auth.DeviceState{
		Brand:  "my-brand",
		Model:  "my-old-model",
		Serial: "serialserial",
	})

	st.Unlock()
	defer st.Lock()

	// make a new get request to the model endpoint with json as true
	req, err := http.NewRequest("GET", "/v2/model/serial?json=true", nil)
	c.Assert(err, check.IsNil)
	rsp := s.syncReq(c, req, nil, actionIsExpected)
	// get the body and try to unmarshal into modelAssertJSON
	c.Assert(rsp.Result, check.FitsTypeOf, clientutil.ModelAssertJSON{})

	jsonResponse := rsp.Result.(clientutil.ModelAssertJSON)

	// get the architecture key from the headers
	devKey, ok := jsonResponse.Headers["device-key"]
	c.Assert(ok, check.Equals, true)

	// check that the device key in the returned assertion matches what we
	// created above
	c.Assert(devKey, check.FitsTypeOf, "")
	c.Assert(devKey.(string), check.Equals, string(encDevKey))
}

func (s *userSuite) TestPostSerialBadAction(c *check.C) {
	buf := bytes.NewBufferString(`{"action":"what"}`)
	req, err := http.NewRequest("POST", "/v2/model/serial", buf)
	c.Assert(err, check.IsNil)

	rspe := s.errorReq(c, req, nil, actionIsUnexpected)
	c.Check(rspe, check.DeepEquals, daemon.BadRequest(`unsupported serial action "what"`))
}

func (s *userSuite) TestPostSerialForget(c *check.C) {
	unregister := 0
	defer daemon.MockDevicestateDeviceManagerUnregister(func(mgr *devicestate.DeviceManager, opts *devicestate.UnregisterOptions) error {
		unregister++
		c.Check(mgr, check.NotNil)
		c.Check(opts.NoRegistrationUntilReboot, check.Equals, false)
		return nil
	})()

	buf := bytes.NewBufferString(`{"action":"forget"}`)
	req, err := http.NewRequest("POST", "/v2/model/serial", buf)
	c.Assert(err, check.IsNil)

	rsp := s.syncReq(c, req, nil, actionIsExpected)
	c.Check(rsp.Result, check.IsNil)

	c.Check(unregister, check.Equals, 1)
}

func (s *userSuite) TestPostSerialForgetNoRegistrationUntilReboot(c *check.C) {
	unregister := 0
	defer daemon.MockDevicestateDeviceManagerUnregister(func(mgr *devicestate.DeviceManager, opts *devicestate.UnregisterOptions) error {
		unregister++
		c.Check(mgr, check.NotNil)
		c.Check(opts.NoRegistrationUntilReboot, check.Equals, true)
		return nil
	})()

	buf := bytes.NewBufferString(`{"action":"forget", "no-registration-until-reboot": true}`)
	req, err := http.NewRequest("POST", "/v2/model/serial", buf)
	c.Assert(err, check.IsNil)

	rsp := s.syncReq(c, req, nil, actionIsExpected)
	c.Check(rsp.Result, check.IsNil)

	c.Check(unregister, check.Equals, 1)
}

func (s *userSuite) TestPostSerialForgetError(c *check.C) {
	defer daemon.MockDevicestateDeviceManagerUnregister(func(mgr *devicestate.DeviceManager, opts *devicestate.UnregisterOptions) error {
		return errors.New("boom")
	})()

	buf := bytes.NewBufferString(`{"action":"forget"}`)
	req, err := http.NewRequest("POST", "/v2/model/serial", buf)
	c.Assert(err, check.IsNil)

	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rspe, check.DeepEquals, daemon.InternalError(`forgetting serial failed: boom`))
}

func multipartBody(c *check.C, model, snap, assertion string) (bytes.Buffer, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	err := w.WriteField("new-model", model)
	c.Assert(err, check.IsNil)
	part, err := w.CreateFormFile("snap", "snap_1.snap")
	c.Assert(err, check.IsNil)
	_, err = part.Write([]byte(snap))
	c.Assert(err, check.IsNil)
	err = w.WriteField("assertion", assertion)
	c.Assert(err, check.IsNil)
	err = w.Close()
	c.Assert(err, check.IsNil)

	return b, w.Boundary()
}

func (s *modelSuite) TestPostOfflineRemodelOk(c *check.C) {
	s.testPostOfflineRemodel(c, &testPostOfflineRemodelParams{badModel: false})
}

func (s *modelSuite) TestPostOfflineRemodelBadModel(c *check.C) {
	s.testPostOfflineRemodel(c, &testPostOfflineRemodelParams{badModel: true})
}

type testPostOfflineRemodelParams struct {
	badModel bool
}

func (s *modelSuite) testPostOfflineRemodel(c *check.C, params *testPostOfflineRemodelParams) {
	s.expectRootAccess()

	oldModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)
	newModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults, map[string]any{
		"revision": "2",
	})

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)
	st := d.Overlord().State()
	st.Lock()
	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, oldModel)
	st.Unlock()

	soon := 0
	var origEnsureStateSoon func(*state.State)
	origEnsureStateSoon, restore := daemon.MockEnsureStateSoon(func(st *state.State) {
		soon++
		origEnsureStateSoon(st)
	})
	defer restore()

	snapName := "snap1"
	snapRev := 1001
	var devicestateRemodelGotModel *asserts.Model
	defer daemon.MockDevicestateRemodel(func(st *state.State, nm *asserts.Model, opts devicestate.RemodelOptions) (*state.Change, error) {
		c.Check(opts.Offline, check.Equals, true)
		c.Check(len(opts.LocalSnaps), check.Equals, 1)
		c.Check(opts.LocalSnaps[0].SideInfo.RealName, check.Equals, snapName)
		c.Check(opts.LocalSnaps[0].SideInfo.Revision, check.Equals, snap.Revision{N: snapRev})
		c.Check(strings.HasSuffix(opts.LocalSnaps[0].Path,
			"/var/lib/snapd/snaps/"+snapName+"_"+strconv.Itoa(snapRev)+".snap"),
			check.Equals, true)

		devicestateRemodelGotModel = nm
		chg := st.NewChange("remodel", "...")
		return chg, nil
	})()

	infos := []*snap.Info{{SideInfo: snap.SideInfo{
		RealName: snapName,
		Revision: snap.R(snapRev),
	}}}

	defer daemon.MockSideloadSnapsInfo(infos)()

	// create a valid model assertion
	c.Assert(err, check.IsNil)
	var modelEncoded string
	if params.badModel {
		modelEncoded = "garbage"
	} else {
		modelEncoded = string(asserts.Encode(newModel))
	}

	// valid revision assertion to make it part of the arguments
	revAssert := assertstest.FakeAssertion(map[string]any{
		"type":          "snap-revision",
		"authority-id":  "can0nical",
		"snap-id":       "snap-id-1",
		"snap-sha3-384": "1111111111111111111111111111111111111111111111111111111111111111",
		"snap-size":     fmt.Sprintf("%d", 100),
		"snap-revision": strconv.Itoa(snapRev),
		"developer-id":  "mydev",
		"timestamp":     time.Now().Format(time.RFC3339),
	}).(*asserts.SnapRevision)

	// create multipart data
	body, boundary := multipartBody(c, modelEncoded, "snap_data", string(revAssert.Body()))

	// set it and validate that this is what we passed to devicestateRemodel
	req, err := http.NewRequest("POST", "/v2/model", &body)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(body.Len()))

	if params.badModel {
		rsp := s.errorReq(c, req, nil, actionIsExpected)
		c.Assert(rsp.Status, check.Equals, 400)
		c.Check(rsp.Error(), check.Equals, "cannot decode new model assertion: assertion content/signature separator not found (api)")
	} else {
		rsp := s.asyncReq(c, req, nil, actionIsExpected)
		c.Assert(rsp.Status, check.Equals, 202)
		c.Check(rsp.Change, check.DeepEquals, "1")
		c.Check(devicestateRemodelGotModel, check.DeepEquals, newModel)

		st.Lock()
		defer st.Unlock()
		chg := st.Change(rsp.Change)
		c.Assert(chg, check.NotNil)

		c.Assert(st.Changes(), check.HasLen, 1)
		chg1 := st.Changes()[0]
		c.Assert(chg, check.DeepEquals, chg1)
		c.Assert(chg.Kind(), check.Equals, "remodel")
		c.Assert(chg.Err(), check.IsNil)

		c.Assert(soon, check.Equals, 1)
	}
}

func (s *modelSuite) TestPostOfflineRemodelWithComponents(c *check.C) {
	s.expectRootAccess()

	oldModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults)
	newModel := s.Brands.Model("my-brand", "my-old-model", modelDefaults, map[string]any{
		"revision": "2",
	})

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	deviceMgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(deviceMgr)

	st := d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	st.Set("seeded", true)

	assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
	assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
	s.mockModel(st, oldModel)

	signer := assertstest.NewStoreStack("can0nical", nil)
	assertstatetest.AddMany(st, signer.StoreAccountKey(""))

	account := assertstest.NewAccount(signer, "developer1", nil, "")
	c.Assert(signer.Add(account), check.IsNil)

	const snapName = "some-snap"
	snapID := snaptest.AssertedSnapID(snapName)
	snapFormData := make(map[string]string)

	snapPath := snaptest.MakeTestSnapWithFiles(c, withComponents("name: some-snap\nversion: 1", []string{"comp"}), nil)
	digest, size, err := asserts.SnapFileSHA3_384(snapPath)
	c.Assert(err, check.IsNil)

	content, err := os.ReadFile(snapPath)
	c.Assert(err, check.IsNil)
	snapFormData[filepath.Base(snapPath)] = string(content)

	rev := mockStoreAssertion(c, signer, signer.AuthorityID, account.AccountID(), asserts.SnapRevisionType, map[string]any{
		"snap-id":       snapID,
		"snap-sha3-384": digest,
		"developer-id":  account.AccountID(),
		"snap-size":     strconv.Itoa(int(size)),
		"snap-revision": "10",
	})

	decl := mockStoreAssertion(c, signer, signer.AuthorityID, account.AccountID(), asserts.SnapDeclarationType, map[string]any{
		"series":       "16",
		"snap-id":      snapID,
		"snap-name":    snapName,
		"publisher-id": account.AccountID(),
	})

	compPath, resRev, resPair := makeStandardComponent(c, signer, signer.AuthorityID, account.AccountID(), snapName, "comp")
	content, err = os.ReadFile(compPath)
	c.Assert(err, check.IsNil)
	snapFormData[filepath.Base(compPath)] = string(content)

	// we handle components that are not associated with any of the snaps that
	// are being uploaded a little differently, this part of the test helps
	// cover that case
	extraSnapDecl := mockStoreAssertion(c, signer, signer.AuthorityID, account.AccountID(), asserts.SnapDeclarationType, map[string]any{
		"series":       "16",
		"snap-id":      snaptest.AssertedSnapID("other-snap"),
		"snap-name":    "other-snap",
		"publisher-id": account.AccountID(),
	})
	extraCompPath, extraResRev, extraResPair := makeStandardComponent(c, signer, signer.AuthorityID, account.AccountID(), "other-snap", "comp")
	content, err = os.ReadFile(extraCompPath)
	c.Assert(err, check.IsNil)
	snapFormData[filepath.Base(extraCompPath)] = string(content)

	snapstate.Set(st, "other-snap", &snapstate.SnapState{
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{{
			RealName: "other-snap",
			Revision: snap.R(10),
			SnapID:   snaptest.AssertedSnapID("other-snap"),
		}}),
		Current: snap.R(10),
		Active:  true,
	})

	var assertions strings.Builder
	encoder := asserts.NewEncoder(&assertions)

	for _, a := range []asserts.Assertion{rev, decl, resRev, resPair, extraSnapDecl, extraResRev, extraResPair, account} {
		err := encoder.Encode(a)
		c.Assert(err, check.IsNil)
	}

	fields := map[string][]string{
		"new-model": {string(asserts.Encode(newModel))},
		"assertion": {assertions.String()},
	}
	form, boundary := createFormData(c, fields, snapFormData)

	defer daemon.MockDevicestateRemodel(func(st *state.State, nm *asserts.Model, opts devicestate.RemodelOptions) (*state.Change, error) {
		c.Check(opts.Offline, check.Equals, true)
		c.Assert(len(opts.LocalSnaps), check.Equals, 1)
		c.Check(opts.LocalSnaps[0].SideInfo.RealName, check.Equals, snapName)
		c.Check(opts.LocalSnaps[0].SideInfo.Revision, check.Equals, snap.Revision{N: 10})

		snapPath := filepath.Join(dirs.SnapBlobDir, "some-snap_10.snap")
		c.Check(strings.HasSuffix(opts.LocalSnaps[0].Path, snapPath), check.Equals, true)

		c.Assert(len(opts.LocalComponents), check.Equals, 2)
		compPath := filepath.Join(dirs.SnapBlobDir, "some-snap+comp_20.comp")
		c.Check(strings.HasSuffix(opts.LocalComponents[0].Path, compPath), check.Equals, true)

		extraCompPath := filepath.Join(dirs.SnapBlobDir, "other-snap+comp_20.comp")
		c.Check(strings.HasSuffix(opts.LocalComponents[1].Path, extraCompPath), check.Equals, true)

		c.Check(nm, check.DeepEquals, newModel)

		chg := st.NewChange("remodel", "...")
		return chg, nil
	})()

	req, err := http.NewRequest("POST", "/v2/model", &form)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(form.Len()))

	st.Unlock()
	rsp := s.asyncReq(c, req, nil, actionIsExpected)
	st.Lock()

	c.Assert(rsp.Status, check.Equals, 202)
	c.Check(rsp.Change, check.DeepEquals, "1")

	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)

	c.Assert(st.Changes(), check.HasLen, 1)
	chg1 := st.Changes()[0]
	c.Assert(chg, check.DeepEquals, chg1)
	c.Assert(chg.Kind(), check.Equals, "remodel")
	c.Assert(chg.Err(), check.IsNil)
}
