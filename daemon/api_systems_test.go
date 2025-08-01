// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2020-2021 Canonical Ltd
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
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/check.v1"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/assertstest"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/bootloader/bootloadertest"
	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/daemon"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/gadget"
	"github.com/snapcore/snapd/gadget/device"
	"github.com/snapcore/snapd/gadget/quantity"
	"github.com/snapcore/snapd/overlord/assertstate/assertstatetest"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/devicestate"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/install"
	"github.com/snapcore/snapd/overlord/restart"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/secboot"
	"github.com/snapcore/snapd/secboot/keys"
	"github.com/snapcore/snapd/seed"
	"github.com/snapcore/snapd/seed/seedtest"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaptest"
)

var _ = check.Suite(&systemsSuite{})

type systemsSuite struct {
	apiBaseSuite

	seedModelForLabel20191119 *asserts.Model
}

func (s *systemsSuite) mockHybridSystem() {
	restore := release.MockReleaseInfo(&release.OS{
		ID:        "ubuntu",
		VersionID: "25.10",
	})
	s.AddCleanup(restore)

	model := s.Brands.Model("can0nical", "pc-new", map[string]any{
		"classic":      "true",
		"distribution": "ubuntu",
		"architecture": "amd64",
		"base":         "core24",
		"snaps": []any{
			map[string]any{
				"name": "pc-kernel",
				"id":   snaptest.AssertedSnapID("pc-kernel"),
				"type": "kernel",
			},
			map[string]any{
				"name": "pc",
				"id":   snaptest.AssertedSnapID("pc"),
				"type": "gadget",
			},
		},
	})
	restore = snapstatetest.MockDeviceModel(model)
	s.AddCleanup(restore)
}

func (s *systemsSuite) SetUpTest(c *check.C) {
	s.apiBaseSuite.SetUpTest(c)

	s.expectRootAccess()
}

var pcGadgetUCYaml = `
volumes:
  pc:
    bootloader: grub
    schema: gpt
    structure:
      - name: mbr
        type: mbr
        size: 440
        content:
          - image: pc-boot.img
      - name: BIOS Boot
        type: DA,21686148-6449-6E6F-744E-656564454649
        size: 1M
        offset: 1M
        offset-write: mbr+92
        content:
          - image: pc-core.img
      - name: ubuntu-seed
        role: system-seed
        filesystem: vfat
        type: EF,C12A7328-F81F-11D2-BA4B-00A0C93EC93B
        size: 1200M
        content:
          - source: grubx64.efi
            target: EFI/boot/grubx64.efi
          - source: shim.efi.signed
            target: EFI/boot/bootx64.efi
      - name: ubuntu-boot
        filesystem: ext4
        size: 750M
        type: 83,0FC63DAF-8483-4772-8E79-3D69D8477DE4
        role: system-boot
      - name: ubuntu-save
        size: 16M
        filesystem: ext4
        type: 83,0FC63DAF-8483-4772-8E79-3D69D8477DE4
        role: system-save
      - name: ubuntu-data
        filesystem: ext4
        size: 1G
        type: 83,0FC63DAF-8483-4772-8E79-3D69D8477DE4
        role: system-data
`

func (s *systemsSuite) mockSystemSeeds(c *check.C) (restore func()) {
	// now create a minimal uc20 seed dir with snaps/assertions
	seed20 := &seedtest.TestingSeed20{
		SeedSnaps: seedtest.SeedSnaps{
			StoreSigning: s.StoreSigning,
			Brands:       s.Brands,
		},
		SeedDir: dirs.SnapSeedDir,
	}

	restore = seed.MockTrusted(seed20.StoreSigning.Trusted)

	assertstest.AddMany(s.StoreSigning.Database, s.Brands.AccountsAndKeys("my-brand")...)
	// add essential snaps
	seed20.MakeAssertedSnap(c, "name: snapd\nversion: 1\ntype: snapd", [][]string{{"usr/lib/snapd/info", "VERSION=1"}}, snap.R(1), "my-brand", s.StoreSigning.Database)
	gadgetFiles := [][]string{
		{"meta/gadget.yaml", string(pcGadgetUCYaml)},
		{"pc-boot.img", "pc-boot.img content"},
		{"pc-core.img", "pc-core.img content"},
		{"grubx64.efi", "grubx64.efi content"},
		{"shim.efi.signed", "shim.efi.signed content"},
	}
	seed20.MakeAssertedSnap(c, "name: pc\nversion: 1\ntype: gadget\nbase: core20", gadgetFiles, snap.R(1), "my-brand", s.StoreSigning.Database)
	seed20.MakeAssertedSnap(c, "name: pc-kernel\nversion: 1\ntype: kernel", nil, snap.R(1), "my-brand", s.StoreSigning.Database)
	seed20.MakeAssertedSnap(c, "name: core20\nversion: 1\ntype: base", nil, snap.R(1), "my-brand", s.StoreSigning.Database)
	s.seedModelForLabel20191119 = seed20.MakeSeed(c, "20191119", "my-brand", "my-model", map[string]any{
		"display-name": "my fancy model",
		"architecture": "amd64",
		"base":         "core20",
		"snaps": []any{
			map[string]any{
				"name": "snapd",
				"id":   seed20.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			map[string]any{
				"name":            "pc-kernel",
				"id":              seed20.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]any{
				"name":            "pc",
				"id":              seed20.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			}},
	}, nil)
	seed20.MakeSeed(c, "20200318", "my-brand", "my-model-2", map[string]any{
		"display-name": "same brand different model",
		"architecture": "amd64",
		"base":         "core20",
		"snaps": []any{
			map[string]any{
				"name": "snapd",
				"id":   seed20.AssertedSnapID("snapd"),
				"type": "snapd",
			},
			map[string]any{
				"name":            "pc-kernel",
				"id":              seed20.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]any{
				"name":            "pc",
				"id":              seed20.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			}},
	}, nil)

	return restore
}

func (s *systemsSuite) TestSystemsGetSome(c *check.C) {
	m := boot.Modeenv{
		Mode: "run",
	}
	err := m.WriteTo("")
	c.Assert(err, check.IsNil)

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	mgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(mgr)

	st := d.Overlord().State()
	st.Lock()
	st.Set("seeded-systems", []map[string]any{{
		"system": "20200318", "model": "my-model-2", "brand-id": "my-brand",
		"revision": 2, "timestamp": "2009-11-10T23:00:00Z",
		"seed-time": "2009-11-10T23:00:00Z",
	}})
	st.Set("default-recovery-system", devicestate.DefaultRecoverySystem{
		System:   "20200318",
		Model:    "my-model-2",
		BrandID:  "my-brand",
		Revision: 2,
	})
	st.Unlock()

	s.expectAuthenticatedAccess()

	restore := s.mockSystemSeeds(c)
	defer restore()

	req, err := http.NewRequest("GET", "/v2/systems", nil)
	c.Assert(err, check.IsNil)
	rsp := s.syncReq(c, req, nil, actionIsExpected)

	c.Assert(rsp.Status, check.Equals, 200)
	sys := rsp.Result.(*daemon.SystemsResponse)

	c.Assert(sys, check.DeepEquals, &daemon.SystemsResponse{
		Systems: []client.System{
			{
				Current: false,
				Label:   "20191119",
				Model: client.SystemModelData{
					Model:       "my-model",
					BrandID:     "my-brand",
					DisplayName: "my fancy model",
				},
				Brand: snap.StoreAccount{
					ID:          "my-brand",
					Username:    "my-brand",
					DisplayName: "My-brand",
					Validation:  "unproven",
				},
				Actions: []client.SystemAction{
					{Title: "Install", Mode: "install"},
					{Title: "Recover", Mode: "recover"},
					{Title: "Factory reset", Mode: "factory-reset"},
				},
			}, {
				Current:               true,
				DefaultRecoverySystem: true,
				Label:                 "20200318",
				Model: client.SystemModelData{
					Model:       "my-model-2",
					BrandID:     "my-brand",
					DisplayName: "same brand different model",
				},
				Brand: snap.StoreAccount{
					ID:          "my-brand",
					Username:    "my-brand",
					DisplayName: "My-brand",
					Validation:  "unproven",
				},
				Actions: []client.SystemAction{
					{Title: "Reinstall", Mode: "install"},
					{Title: "Recover", Mode: "recover"},
					{Title: "Factory reset", Mode: "factory-reset"},
					{Title: "Run normally", Mode: "run"},
				},
			},
		}})
}

func (s *systemsSuite) TestSystemsGetNone(c *check.C) {
	m := boot.Modeenv{
		Mode: "run",
	}
	err := m.WriteTo("")
	c.Assert(err, check.IsNil)

	// model assertion setup
	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	mgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(mgr)

	s.expectAuthenticatedAccess()

	// no system seeds
	req, err := http.NewRequest("GET", "/v2/systems", nil)
	c.Assert(err, check.IsNil)
	rsp := s.syncReq(c, req, nil, actionIsExpected)

	c.Assert(rsp.Status, check.Equals, 200)
	sys := rsp.Result.(*daemon.SystemsResponse)

	c.Assert(sys, check.DeepEquals, &daemon.SystemsResponse{})
}

func (s *systemsSuite) TestSystemActionRequestErrors(c *check.C) {
	// modeenv must be mocked before daemon is initialized
	m := boot.Modeenv{
		Mode: "run",
	}
	err := m.WriteTo("")
	c.Assert(err, check.IsNil)

	d := s.daemonWithOverlordMockAndStore()

	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	mgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(mgr)

	restore := s.mockSystemSeeds(c)
	defer restore()

	st := d.Overlord().State()

	type table struct {
		label, body, error string
		status             int
		unseeded           bool
	}
	tests := []table{
		{
			label:  "foobar",
			body:   `"bogus"`,
			error:  "cannot decode request body into system action:.*",
			status: 400,
		}, {
			label:  "",
			body:   `{"action":"do","mode":"install"}`,
			error:  "system action requires the system label to be provided",
			status: 400,
		}, {
			label:  "foobar",
			body:   `{"action":"do"}`,
			error:  "system action requires the mode to be provided",
			status: 400,
		}, {
			label:  "foobar",
			body:   `{"action":"nope","mode":"install"}`,
			error:  `unsupported action "nope"`,
			status: 400,
		}, {
			label:  "foobar",
			body:   `{"action":"do","mode":"install"}`,
			error:  `requested seed system "foobar" does not exist`,
			status: 404,
		}, {
			// valid system label but incorrect action
			label:  "20191119",
			body:   `{"action":"do","mode":"foobar"}`,
			error:  `requested action is not supported by system "20191119"`,
			status: 400,
		}, {
			// valid label and action, but seeding is not complete yet
			label:    "20191119",
			body:     `{"action":"do","mode":"install"}`,
			error:    `cannot request system action, system is seeding`,
			status:   500,
			unseeded: true,
		},
	}
	for _, tc := range tests {
		st.Lock()
		if tc.unseeded {
			st.Set("seeded", nil)
			m := boot.Modeenv{
				Mode:           "run",
				RecoverySystem: tc.label,
			}
			err := m.WriteTo("")
			c.Assert(err, check.IsNil)
		} else {
			st.Set("seeded", true)
		}
		st.Unlock()
		c.Logf("tc: %#v", tc)
		req, err := http.NewRequest("POST", path.Join("/v2/systems", tc.label), strings.NewReader(tc.body))
		c.Assert(err, check.IsNil)
		rspe := s.errorReq(c, req, nil, actionExpectedBool(
			!strings.Contains(tc.error, "unsupported action") &&
				!strings.Contains(tc.error, "system action requires the system label to be provided")))
		c.Check(rspe.Status, check.Equals, tc.status)
		c.Check(rspe.Message, check.Matches, tc.error)
	}
}

func (s *systemsSuite) TestSystemActionRequestWithSeeded(c *check.C) {
	bt := bootloadertest.Mock("mock", c.MkDir())
	bootloader.Force(bt)
	defer func() { bootloader.Force(nil) }()

	nRebootCall := 0
	rebootCheck := func(ra boot.RebootAction, d time.Duration, ri *boot.RebootInfo) error {
		nRebootCall++
		// slow reboot schedule
		c.Check(ra, check.Equals, boot.RebootReboot)
		c.Check(d, check.Equals, 10*time.Minute)
		c.Check(ri, check.IsNil)
		return nil
	}
	r := daemon.MockReboot(rebootCheck)
	defer r()

	restore := s.mockSystemSeeds(c)
	defer restore()

	model := s.Brands.Model("my-brand", "pc", map[string]any{
		"architecture": "amd64",
		// UC20
		"grade": "dangerous",
		"base":  "core20",
		"snaps": []any{
			map[string]any{
				"name":            "pc-kernel",
				"id":              snaptest.AssertedSnapID("pc-kernel"),
				"type":            "kernel",
				"default-channel": "20",
			},
			map[string]any{
				"name":            "pc",
				"id":              snaptest.AssertedSnapID("pc"),
				"type":            "gadget",
				"default-channel": "20",
			},
		},
	})

	currentSystem := []map[string]any{{
		"system": "20191119", "model": "my-model", "brand-id": "my-brand",
		"revision": 2, "timestamp": "2009-11-10T23:00:00Z",
		"seed-time": "2009-11-10T23:00:00Z",
	}}

	numExpRestart := 0
	tt := []struct {
		currentMode    string
		actionMode     string
		expUnsupported bool
		expRestart     bool
		comment        string
	}{
		{
			// from run mode -> install mode works to reinstall the system
			currentMode: "run",
			actionMode:  "install",
			expRestart:  true,
			comment:     "run mode to install mode",
		},
		{
			// from run mode -> recover mode works to recover the system
			currentMode: "run",
			actionMode:  "recover",
			expRestart:  true,
			comment:     "run mode to recover mode",
		},
		{
			// from run mode -> run mode is no-op
			currentMode: "run",
			actionMode:  "run",
			comment:     "run mode to run mode",
		},
		{
			// from run mode -> factory-reset
			currentMode: "run",
			actionMode:  "factory-reset",
			expRestart:  true,
			comment:     "run mode to factory-reset mode",
		},
		{
			// from recover mode -> run mode works to stop
			// recovering and "restore" the system to normal
			currentMode: "recover",
			actionMode:  "run",
			expRestart:  true,
			comment:     "recover mode to run mode",
		},
		{
			// from recover mode -> install mode works to stop
			// recovering and reinstall the system if all is lost
			currentMode: "recover",
			actionMode:  "install",
			expRestart:  true,
			comment:     "recover mode to install mode",
		},
		{
			// from recover mode -> recover mode is no-op
			currentMode:    "recover",
			actionMode:     "recover",
			expUnsupported: true,
			comment:        "recover mode to recover mode",
		},
		{
			// from recover mode -> factory-reset works
			currentMode: "recover",
			actionMode:  "factory-reset",
			expRestart:  true,
			comment:     "recover mode to factory-reset mode",
		},
		{
			// from install mode -> install mode is no-no
			currentMode:    "install",
			actionMode:     "install",
			expUnsupported: true,
			comment:        "install mode to install mode not supported",
		},
		{
			// from install mode -> run mode is no-no
			currentMode:    "install",
			actionMode:     "run",
			expUnsupported: true,
			comment:        "install mode to run mode not supported",
		},
		{
			// from install mode -> recover mode is no-no
			currentMode:    "install",
			actionMode:     "recover",
			expUnsupported: true,
			comment:        "install mode to recover mode not supported",
		},
	}

	for _, tc := range tt {
		c.Logf("tc: %v", tc.comment)
		// daemon setup - need to do this per-test because we need to re-read
		// the modeenv during devicemgr startup
		m := boot.Modeenv{
			Mode: tc.currentMode,
		}
		if tc.currentMode != "run" {
			m.RecoverySystem = "20191119"
		}
		err := m.WriteTo("")
		c.Assert(err, check.IsNil)
		d := s.daemon(c)
		st := d.Overlord().State()
		st.Lock()
		// make things look like a reboot
		restart.ReplaceBootID(st, "boot-id-1")
		// device model
		assertstatetest.AddMany(st, s.StoreSigning.StoreAccountKey(""))
		assertstatetest.AddMany(st, s.Brands.AccountsAndKeys("my-brand")...)
		s.mockModel(st, model)
		if tc.currentMode == "run" {
			// only set in run mode
			st.Set("seeded-systems", currentSystem)
		}
		// the seeding is done
		st.Set("seeded", true)
		st.Unlock()

		body := map[string]string{
			"action": "do",
			"mode":   tc.actionMode,
		}
		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil, check.Commentf(tc.comment))
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
		c.Assert(err, check.IsNil, check.Commentf(tc.comment))
		// as root
		s.asRootAuth(req)
		rec := httptest.NewRecorder()
		s.serveHTTP(c, rec, req)
		if tc.expUnsupported {
			c.Check(rec.Code, check.Equals, 400, check.Commentf(tc.comment))
		} else {
			c.Check(rec.Code, check.Equals, 200, check.Commentf(tc.comment))
		}

		var rspBody map[string]any
		err = json.Unmarshal(rec.Body.Bytes(), &rspBody)
		c.Assert(err, check.IsNil, check.Commentf(tc.comment))

		var expResp map[string]any
		if tc.expUnsupported {
			expResp = map[string]any{
				"result": map[string]any{
					"message": fmt.Sprintf("requested action is not supported by system %q", "20191119"),
				},
				"status":      "Bad Request",
				"status-code": 400.0,
				"type":        "error",
			}
		} else {
			expResp = map[string]any{
				"result":      nil,
				"status":      "OK",
				"status-code": 200.0,
				"type":        "sync",
			}
			if tc.expRestart {
				expResp["maintenance"] = map[string]any{
					"kind":    "system-restart",
					"message": "system is restarting",
					"value": map[string]any{
						"op": "reboot",
					},
				}

				// daemon is not started, only check whether reboot was scheduled as expected

				// reboot flag
				numExpRestart++
				c.Check(d.RequestedRestart(), check.Equals, restart.RestartSystemNow, check.Commentf(tc.comment))
			}
		}

		c.Assert(rspBody, check.DeepEquals, expResp, check.Commentf(tc.comment))

		s.resetDaemon()
	}

	// we must have called reboot numExpRestart times
	c.Check(nRebootCall, check.Equals, numExpRestart)
}

func (s *systemsSuite) TestSystemActionBrokenSeed(c *check.C) {
	m := boot.Modeenv{
		Mode: "run",
	}
	err := m.WriteTo("")
	c.Assert(err, check.IsNil)

	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	mgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(mgr)

	// the seeding is done
	st := d.Overlord().State()
	st.Lock()
	st.Set("seeded", true)
	st.Unlock()

	restore := s.mockSystemSeeds(c)
	defer restore()

	err = os.Remove(filepath.Join(dirs.SnapSeedDir, "systems", "20191119", "model"))
	c.Assert(err, check.IsNil)

	body := `{"action":"do","title":"reinstall","mode":"install"}`
	req, err := http.NewRequest("POST", "/v2/systems/20191119", strings.NewReader(body))
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rspe.Status, check.Equals, 500)
	c.Check(rspe.Message, check.Matches, `cannot load seed system: cannot load assertions for label "20191119": .*`)
}

func (s *systemsSuite) TestSystemActionNonRoot(c *check.C) {
	d := s.daemonWithOverlordMockAndStore()
	hookMgr, err := hookstate.Manager(d.Overlord().State(), d.Overlord().TaskRunner())
	c.Assert(err, check.IsNil)
	mgr, err := devicestate.Manager(d.Overlord().State(), hookMgr, d.Overlord().TaskRunner(), nil)
	c.Assert(err, check.IsNil)
	d.Overlord().AddManager(mgr)

	body := `{"action":"do","title":"reinstall","mode":"install"}`

	// pretend to be a simple user
	req, err := http.NewRequest("POST", "/v2/systems/20191119", strings.NewReader(body))
	c.Assert(err, check.IsNil)
	// non root
	s.asUserAuth(c, req)

	rec := httptest.NewRecorder()
	s.serveHTTP(c, rec, req)
	c.Assert(rec.Code, check.Equals, 403)

	var rspBody map[string]any
	err = json.Unmarshal(rec.Body.Bytes(), &rspBody)
	c.Check(err, check.IsNil)
	c.Check(rspBody, check.DeepEquals, map[string]any{
		"result": map[string]any{
			"message": "access denied",
			"kind":    "login-required",
		},
		"status":      "Forbidden",
		"status-code": 403.0,
		"type":        "error",
	})
}

func (s *systemsSuite) TestSystemRebootNeedsRoot(c *check.C) {
	s.daemon(c)

	restore := daemon.MockDeviceManagerReboot(func(dm *devicestate.DeviceManager, systemLabel, mode string) error {
		c.Fatalf("request reboot should not get called")
		return nil
	})
	defer restore()

	body := `{"action":"reboot"}`
	url := "/v2/systems"
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	c.Assert(err, check.IsNil)
	// non root
	s.asUserAuth(c, req)

	rec := httptest.NewRecorder()
	s.serveHTTP(c, rec, req)
	c.Check(rec.Code, check.Equals, 403)
}

func (s *systemsSuite) TestSystemRebootHappy(c *check.C) {
	s.daemon(c)

	for _, tc := range []struct {
		systemLabel, mode string
	}{
		{"", ""},
		{"20200101", ""},
		{"", "run"},
		{"", "recover"},
		{"", "factory-reset"},
		{"20200101", "run"},
		{"20200101", "recover"},
		{"20200101", "factory-reset"},
	} {
		called := 0
		restore := daemon.MockDeviceManagerReboot(func(dm *devicestate.DeviceManager, systemLabel, mode string) error {
			called++
			c.Check(dm, check.NotNil)
			c.Check(systemLabel, check.Equals, tc.systemLabel)
			c.Check(mode, check.Equals, tc.mode)
			return nil
		})
		defer restore()

		body := fmt.Sprintf(`{"action":"reboot", "mode":"%s"}`, tc.mode)
		url := "/v2/systems"
		if tc.systemLabel != "" {
			url += "/" + tc.systemLabel
		}
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		c.Assert(err, check.IsNil)
		s.asRootAuth(req)

		rec := httptest.NewRecorder()
		s.serveHTTP(c, rec, req)
		c.Check(rec.Code, check.Equals, 200)
		c.Check(called, check.Equals, 1)
	}
}

func (s *systemsSuite) TestSystemRebootUnhappy(c *check.C) {
	s.daemon(c)

	for _, tc := range []struct {
		rebootErr        error
		expectedHttpCode int
		expectedErr      string
	}{
		{fmt.Errorf("boom"), 500, "boom"},
		{os.ErrNotExist, 404, `requested seed system "" does not exist`},
		{devicestate.ErrUnsupportedAction, 400, `requested action is not supported by system ""`},
	} {
		called := 0
		restore := daemon.MockDeviceManagerReboot(func(dm *devicestate.DeviceManager, systemLabel, mode string) error {
			called++
			return tc.rebootErr
		})
		defer restore()

		body := `{"action":"reboot"}`
		url := "/v2/systems"
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
		c.Assert(err, check.IsNil)
		s.asRootAuth(req)

		rec := httptest.NewRecorder()
		s.serveHTTP(c, rec, req)
		c.Check(rec.Code, check.Equals, tc.expectedHttpCode)
		c.Check(called, check.Equals, 1)

		var rspBody map[string]any
		err = json.Unmarshal(rec.Body.Bytes(), &rspBody)
		c.Check(err, check.IsNil)
		c.Check(rspBody["status-code"], check.Equals, float64(tc.expectedHttpCode))
		result := rspBody["result"].(map[string]any)
		c.Check(result["message"], check.Equals, tc.expectedErr)
	}
}

// XXX: duplicated from gadget_test.go
func asOffsetPtr(offs quantity.Offset) *quantity.Offset {
	goff := offs
	return &goff
}

// This combination of UnavailableWarning and AvailabilityCheckErrors represents the
// Ubuntu 25.10+ hybrid install using the comprehensive secboot preinstall check to
// determine encryption availability. Other systems use the basic availability check
// that will not populate AvailabilityCheckErrors. This test case exercises the superset
// of availability check behavior.
var unavailableWarning string = "not encrypting device storage as checking TPM gave: preinstall check identified 2 errors"
var availabilityCheckErrors = []secboot.PreinstallErrorDetails{
	{
		Kind:    "tpm-hierarchies-owned",
		Message: "error with TPM2 device: one or more of the TPM hierarchies is already owned",
		Args: map[string]json.RawMessage{
			"with-auth-value":  json.RawMessage(`[1073741834]`),
			"with-auth-policy": json.RawMessage(`[1073741825]`),
		},
		Actions: []string{"reboot-to-fw-settings"},
	},
	{
		Kind:    "tpm-device-lockout",
		Message: "error with TPM2 device: TPM is in DA lockout mode",
		Args: map[string]json.RawMessage{
			"interval-duration": json.RawMessage(`7200000000000`),
			"total-duration":    json.RawMessage(`230400000000000`),
		},
		Actions: []string{"reboot-to-fw-settings"},
	},
}

func (s *systemsSuite) TestSystemsGetSystemDetailsForLabel(c *check.C) {
	s.mockSystemSeeds(c)

	s.daemon(c)
	s.expectRootAccess()

	mockGadgetInfo := &gadget.Info{
		Volumes: map[string]*gadget.Volume{
			"pc": {
				Schema:     "gpt",
				Bootloader: "grub",
				Structure: []gadget.VolumeStructure{
					{
						VolumeName: "foo",
					},
				},
			},
		},
	}

	for _, tc := range []struct {
		disabled, available, passphraseAuthAvailable bool
		storageSafety                                asserts.StorageSafety
		typ                                          device.EncryptionType
		unavailableErr, unavailableWarning           string
		availabilityCheckErrs                        []secboot.PreinstallErrorDetails

		expectedSupport                                  client.StorageEncryptionSupport
		expectedStorageSafety, expectedUnavailableReason string
		expectedAvailabilityCheckErrs                    []secboot.PreinstallErrorDetails
		expectedEncryptionFeatures                       []client.StorageEncryptionFeature
	}{
		{
			disabled:      true,
			storageSafety: asserts.StorageSafetyPreferEncrypted,

			expectedSupport: client.StorageEncryptionSupportDisabled,
		},
		{
			storageSafety:      asserts.StorageSafetyPreferEncrypted,
			unavailableWarning: "unavailable-warn",

			expectedSupport:           client.StorageEncryptionSupportUnavailable,
			expectedStorageSafety:     "prefer-encrypted",
			expectedUnavailableReason: "unavailable-warn",
		},
		{
			available:     true,
			storageSafety: asserts.StorageSafetyPreferEncrypted,
			typ:           "cryptsetup",

			expectedSupport:       client.StorageEncryptionSupportAvailable,
			expectedStorageSafety: "prefer-encrypted",
		},
		{
			available:     true,
			storageSafety: asserts.StorageSafetyPreferUnencrypted,
			typ:           "cryptsetup",

			expectedSupport:       client.StorageEncryptionSupportAvailable,
			expectedStorageSafety: "prefer-unencrypted",
		},
		{
			storageSafety:         asserts.StorageSafetyEncrypted,
			unavailableErr:        unavailableWarning,
			availabilityCheckErrs: availabilityCheckErrors,

			expectedSupport:               client.StorageEncryptionSupportDefective,
			expectedStorageSafety:         "encrypted",
			expectedUnavailableReason:     unavailableWarning,
			expectedAvailabilityCheckErrs: availabilityCheckErrors,
		},
		{
			available:               true,
			passphraseAuthAvailable: true,
			storageSafety:           asserts.StorageSafetyEncrypted,

			expectedSupport:            client.StorageEncryptionSupportAvailable,
			expectedStorageSafety:      "encrypted",
			expectedEncryptionFeatures: []client.StorageEncryptionFeature{client.StorageEncryptionFeaturePassphraseAuth},
		},
	} {
		mockEncryptionSupportInfo := &install.EncryptionSupportInfo{
			Available:               tc.available,
			Disabled:                tc.disabled,
			StorageSafety:           tc.storageSafety,
			UnavailableErr:          errors.New(tc.unavailableErr),
			UnavailableWarning:      tc.unavailableWarning,
			PassphraseAuthAvailable: tc.passphraseAuthAvailable,
		}

		r := daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(mgr *devicestate.DeviceManager, label string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
			c.Check(label, check.Equals, "20191119")
			sys := &devicestate.System{
				Model: s.seedModelForLabel20191119,
				Label: "20191119",
				Brand: s.Brands.Account("my-brand"),
				OptionalContainers: devicestate.OptionalContainers{
					Snaps:      []string{"snap1", "snap2"},
					Components: map[string][]string{"snap1": {"comp1"}, "snap2": {"comp2"}},
				},
			}
			return sys, mockGadgetInfo, mockEncryptionSupportInfo, nil
		})
		defer r()

		req, err := http.NewRequest("GET", "/v2/systems/20191119", nil)
		c.Assert(err, check.IsNil)
		rsp := s.syncReq(c, req, nil, actionIsExpected)

		c.Assert(rsp.Status, check.Equals, 200)
		sys := rsp.Result.(client.SystemDetails)
		c.Check(sys, check.DeepEquals, client.SystemDetails{
			Label: "20191119",
			Model: s.seedModelForLabel20191119.Headers(),
			Brand: snap.StoreAccount{
				ID:          "my-brand",
				Username:    "my-brand",
				DisplayName: "My-brand",
				Validation:  "unproven",
			},
			StorageEncryption: &client.StorageEncryption{
				Support:           tc.expectedSupport,
				Features:          tc.expectedEncryptionFeatures,
				StorageSafety:     tc.expectedStorageSafety,
				UnavailableReason: tc.expectedUnavailableReason,
			},
			Volumes: mockGadgetInfo.Volumes,
			AvailableOptional: client.AvailableForInstall{
				Snaps: []string{"snap1", "snap2"},
				Components: map[string][]string{
					"snap1": {"comp1"},
					"snap2": {"comp2"},
				},
			},
		}, check.Commentf("%v", tc))
	}
}

func (s *systemsSuite) TestSystemsGetSpecificLabelError(c *check.C) {
	s.daemon(c)
	s.expectRootAccess()

	r := daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(mgr *devicestate.DeviceManager, label string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
		return nil, nil, nil, fmt.Errorf("boom")
	})
	defer r()

	req, err := http.NewRequest("GET", "/v2/systems/something", nil)
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)

	c.Assert(rspe.Status, check.Equals, 500)
	c.Check(rspe.Message, check.Equals, `boom`)
}

func (s *systemsSuite) TestSystemsGetSpecificLabelNotFoundIntegration(c *check.C) {
	restore := release.MockOnClassic(false)
	defer restore()

	s.daemon(c)
	s.expectRootAccess()

	req, err := http.NewRequest("GET", "/v2/systems/does-not-exist", nil)
	c.Assert(err, check.IsNil)
	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rspe.Status, check.Equals, 500)
	c.Check(rspe.Message, check.Equals, `cannot load assertions for label "does-not-exist": no seed assertions`)
}

func (s *systemsSuite) TestSystemsGetSpecificLabelIntegration(c *check.C) {
	restore := release.MockOnClassic(false)
	defer restore()

	d := s.daemon(c)
	s.expectRootAccess()
	deviceMgr := d.Overlord().DeviceManager()

	restore = s.mockSystemSeeds(c)
	defer restore()

	r := daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(mgr *devicestate.DeviceManager, label string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
		// mockSystemSeed will ensure everything here is coming from
		// the mocked seed except the encryptionInfo
		sys, gadgetInfo, encInfo, err := deviceMgr.SystemAndGadgetAndEncryptionInfo(label)
		c.Assert(err, check.IsNil)
		// encryptionInfo needs get overridden here to get reliable tests
		encInfo.Available = false
		encInfo.StorageSafety = asserts.StorageSafetyPreferEncrypted
		encInfo.UnavailableWarning = unavailableWarning
		encInfo.AvailabilityCheckErrors = availabilityCheckErrors

		return sys, gadgetInfo, encInfo, err
	})
	defer r()

	req, err := http.NewRequest("GET", "/v2/systems/20191119", nil)
	c.Assert(err, check.IsNil)
	rsp := s.syncReq(c, req, nil, actionIsExpected)

	c.Assert(rsp.Status, check.Equals, 200)
	sys := rsp.Result.(client.SystemDetails)

	sd := client.SystemDetails{
		Label: "20191119",
		Model: s.seedModelForLabel20191119.Headers(),
		Actions: []client.SystemAction{
			{Title: "Install", Mode: "install"},
			{Title: "Recover", Mode: "recover"},
			{Title: "Factory reset", Mode: "factory-reset"},
		},

		Brand: snap.StoreAccount{
			ID:          "my-brand",
			Username:    "my-brand",
			DisplayName: "My-brand",
			Validation:  "unproven",
		},
		StorageEncryption: &client.StorageEncryption{
			Support:                 "unavailable",
			StorageSafety:           "prefer-encrypted",
			UnavailableReason:       unavailableWarning,
			AvailabilityCheckErrors: availabilityCheckErrors,
		},
		Volumes: map[string]*gadget.Volume{
			"pc": {
				Name:       "pc",
				Schema:     "gpt",
				Bootloader: "grub",
				Structure: []gadget.VolumeStructure{
					{
						Name:       "mbr",
						VolumeName: "pc",
						Type:       "mbr",
						Role:       "mbr",
						Offset:     asOffsetPtr(0),
						MinSize:    440,
						Size:       440,
						Content: []gadget.VolumeContent{
							{
								Image: "pc-boot.img",
							},
						},
						YamlIndex: 0,
					},
					{
						Name:       "BIOS Boot",
						VolumeName: "pc",
						Type:       "DA,21686148-6449-6E6F-744E-656564454649",
						MinSize:    1 * quantity.SizeMiB,
						Size:       1 * quantity.SizeMiB,
						Offset:     asOffsetPtr(1 * quantity.OffsetMiB),
						OffsetWrite: &gadget.RelativeOffset{
							RelativeTo: "mbr",
							Offset:     92,
						},
						Content: []gadget.VolumeContent{
							{
								Image: "pc-core.img",
							},
						},
						YamlIndex: 1,
					},
					{
						Name:       "ubuntu-seed",
						Label:      "ubuntu-seed",
						Role:       "system-seed",
						VolumeName: "pc",
						Type:       "EF,C12A7328-F81F-11D2-BA4B-00A0C93EC93B",
						Offset:     asOffsetPtr(2 * quantity.OffsetMiB),
						MinSize:    1200 * quantity.SizeMiB,
						Size:       1200 * quantity.SizeMiB,
						Filesystem: "vfat",
						Content: []gadget.VolumeContent{
							{
								UnresolvedSource: "grubx64.efi",
								Target:           "EFI/boot/grubx64.efi",
							},
							{
								UnresolvedSource: "shim.efi.signed",
								Target:           "EFI/boot/bootx64.efi",
							},
						},
						YamlIndex: 2,
					},
					{
						Name:       "ubuntu-boot",
						Label:      "ubuntu-boot",
						Role:       "system-boot",
						VolumeName: "pc",
						Type:       "83,0FC63DAF-8483-4772-8E79-3D69D8477DE4",
						Offset:     asOffsetPtr(1202 * quantity.OffsetMiB),
						MinSize:    750 * quantity.SizeMiB,
						Size:       750 * quantity.SizeMiB,
						Filesystem: "ext4",
						YamlIndex:  3,
					},
					{
						Name:       "ubuntu-save",
						Label:      "ubuntu-save",
						Role:       "system-save",
						VolumeName: "pc",
						Type:       "83,0FC63DAF-8483-4772-8E79-3D69D8477DE4",
						Offset:     asOffsetPtr(1952 * quantity.OffsetMiB),
						MinSize:    16 * quantity.SizeMiB,
						Size:       16 * quantity.SizeMiB,
						Filesystem: "ext4",
						YamlIndex:  4,
					},
					{
						Name:       "ubuntu-data",
						Label:      "ubuntu-data",
						Role:       "system-data",
						VolumeName: "pc",
						Type:       "83,0FC63DAF-8483-4772-8E79-3D69D8477DE4",
						Offset:     asOffsetPtr(1968 * quantity.OffsetMiB),
						MinSize:    1 * quantity.SizeGiB,
						Size:       1 * quantity.SizeGiB,
						Filesystem: "ext4",
						YamlIndex:  5,
					},
				},
			},
		},
	}
	gadget.SetEnclosingVolumeInStructs(sd.Volumes)
	c.Assert(sys, check.DeepEquals, sd)
}

func (s *systemsSuite) TestSystemInstallActionFinishCallsDevicestate(c *check.C) {
	s.testSystemInstallActionFinishCallsDevicestate(c, client.OptionalInstallRequest{
		AvailableForInstall: client.AvailableForInstall{
			Snaps: []string{"snap1", "snap2"},
			Components: map[string][]string{
				"snap1": {"comp1"},
				"snap2": {"comp2"},
			},
		},
	})
}

func (s *systemsSuite) TestSystemInstallActionFinishCallsDevicestateAll(c *check.C) {
	s.testSystemInstallActionFinishCallsDevicestate(c, client.OptionalInstallRequest{
		All: true,
	})
}

func (s *systemsSuite) testSystemInstallActionFinishCallsDevicestate(c *check.C, optionalInstall client.OptionalInstallRequest) {
	d := s.daemon(c)
	st := d.Overlord().State()

	soon := 0
	_, restore := daemon.MockEnsureStateSoon(func(st *state.State) {
		soon++
	})
	defer restore()

	nCalls := 0
	var gotOnVolumes map[string]*gadget.Volume
	var gotLabel string
	var gotOptionalInstall *devicestate.OptionalContainers
	r := daemon.MockDevicestateInstallFinish(func(st *state.State, label string, onVolumes map[string]*gadget.Volume, optionalInstall *devicestate.OptionalContainers) (*state.Change, error) {
		gotLabel = label
		gotOnVolumes = onVolumes
		gotOptionalInstall = optionalInstall
		nCalls++
		return st.NewChange("foo", "..."), nil
	})
	defer r()

	body := map[string]any{
		"action": "install",
		"step":   "finish",
		"on-volumes": map[string]any{
			"pc": map[string]any{
				"bootloader": "grub",
			},
		},
		"optional-install": map[string]any{
			"snaps":      optionalInstall.Snaps,
			"components": optionalInstall.Components,
			"all":        optionalInstall.All,
		},
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
	c.Assert(err, check.IsNil)

	rsp := s.asyncReq(c, req, nil, actionIsExpected)

	st.Lock()
	chg := st.Change(rsp.Change)
	st.Unlock()
	c.Check(chg, check.NotNil)
	c.Check(chg.ID(), check.Equals, "1")
	c.Check(nCalls, check.Equals, 1)
	c.Check(gotLabel, check.Equals, "20191119")
	c.Check(gotOnVolumes, check.DeepEquals, map[string]*gadget.Volume{
		"pc": {
			Bootloader: "grub",
		},
	})

	if optionalInstall.All {
		c.Check(gotOptionalInstall, check.IsNil)
	} else {
		c.Check(gotOptionalInstall, check.DeepEquals, &devicestate.OptionalContainers{
			Snaps:      optionalInstall.Snaps,
			Components: optionalInstall.Components,
		})
	}

	c.Check(soon, check.Equals, 1)
}

func (s *systemsSuite) TestSystemInstallActionFinishCallsDevicestateAllAndSpecificInstallsFails(c *check.C) {
	s.daemon(c)

	body := map[string]any{
		"action": "install",
		"step":   "finish",
		"on-volumes": map[string]any{
			"pc": map[string]any{
				"bootloader": "grub",
			},
		},
		"optional-install": map[string]any{
			"snaps":      []string{"snap1", "snap2"},
			"components": map[string][]string{"snap1": {"comp1"}, "snap2": {"comp2"}},
			"all":        true,
		},
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
	c.Assert(err, check.IsNil)

	rsp := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Message, check.Equals, "cannot specify both all and individual optional snaps and components to install")
}

func (s *systemsSuite) TestSystemInstallActionSetupStorageEncryptionCallsDevicestate(c *check.C) {
	d := s.daemon(c)
	st := d.Overlord().State()

	soon := 0
	_, restore := daemon.MockEnsureStateSoon(func(st *state.State) {
		soon++
	})
	defer restore()

	nCalls := 0
	var gotOnVolumes map[string]*gadget.Volume
	var gotLabel string
	var gotVolumesAuth *device.VolumesAuthOptions
	r := daemon.MockDevicestateInstallSetupStorageEncryption(func(st *state.State, label string, onVolumes map[string]*gadget.Volume, volumesAuth *device.VolumesAuthOptions) (*state.Change, error) {
		gotLabel = label
		gotOnVolumes = onVolumes
		gotVolumesAuth = volumesAuth
		nCalls++
		return st.NewChange("foo", "..."), nil
	})
	defer r()

	body := map[string]any{
		"action": "install",
		"step":   "setup-storage-encryption",
		"on-volumes": map[string]any{
			"pc": map[string]any{
				"bootloader": "grub",
			},
		},
		"volumes-auth": map[string]any{
			"mode":       "passphrase",
			"passphrase": "1234",
		},
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
	c.Assert(err, check.IsNil)

	rsp := s.asyncReq(c, req, nil, actionIsExpected)

	st.Lock()
	chg := st.Change(rsp.Change)
	st.Unlock()
	c.Check(chg, check.NotNil)
	c.Check(chg.ID(), check.Equals, "1")
	c.Check(nCalls, check.Equals, 1)
	c.Check(gotLabel, check.Equals, "20191119")
	c.Check(gotOnVolumes, check.DeepEquals, map[string]*gadget.Volume{
		"pc": {
			Bootloader: "grub",
		},
	})
	c.Check(gotVolumesAuth, check.DeepEquals, &device.VolumesAuthOptions{
		Mode:       device.AuthModePassphrase,
		Passphrase: "1234",
	})

	c.Check(soon, check.Equals, 1)
}

func (s *systemsSuite) TestSystemInstallActionGenerateRecoveryKey(c *check.C) {
	if (keys.RecoveryKey{}).String() == "not-implemented" {
		c.Skip("needs working secboot recovery key")
	}

	s.daemon(c)
	s.mockHybridSystem()

	defer daemon.MockDevicestateGeneratePreInstallRecoveryKey(func(st *state.State, label string) (rkey keys.RecoveryKey, err error) {
		c.Check(label, check.Equals, "20250529")
		return keys.RecoveryKey{'r', 'e', 'c', 'o', 'v', 'e', 'r', 'y', '1', '1', '1', '1', '1', '1', '1', '1'}, nil
	})()

	body := map[string]any{
		"action": "install",
		"step":   "generate-recovery-key",
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20250529", buf)
	c.Assert(err, check.IsNil)

	rsp := s.syncReq(c, req, nil, actionIsExpected)
	c.Assert(rsp.Status, check.Equals, 200)

	res := rsp.Result.(map[string]string)
	c.Check(res, check.DeepEquals, map[string]string{
		"recovery-key": "25970-28515-25974-31090-12593-12593-12593-12593",
	})
}

func (s *systemsSuite) TestSystemInstallActionGenerateRecoveryKeyError(c *check.C) {
	s.daemon(c)
	s.mockHybridSystem()

	defer daemon.MockDevicestateGeneratePreInstallRecoveryKey(func(st *state.State, label string) (rkey keys.RecoveryKey, err error) {
		c.Check(label, check.Equals, "20250529")
		return keys.RecoveryKey{}, errors.New("boom!")
	})()

	body := map[string]any{
		"action": "install",
		"step":   "generate-recovery-key",
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20250529", buf)
	c.Assert(err, check.IsNil)

	rsp := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Message, check.Equals, `cannot generate recovery key for "20250529": boom!`)
}

func (s *systemsSuite) TestSystemInstallActionGeneratesTasks(c *check.C) {
	d := s.daemon(c)
	st := d.Overlord().State()

	var soon int
	_, restore := daemon.MockEnsureStateSoon(func(st *state.State) {
		soon++
	})
	defer restore()

	for _, tc := range []struct {
		installStep      string
		expectedNumTasks int
	}{
		{"finish", 1},
		{"setup-storage-encryption", 1},
	} {
		soon = 0
		body := map[string]any{
			"action": "install",
			"step":   tc.installStep,
			"on-volumes": map[string]any{
				"pc": map[string]any{
					"bootloader": "grub",
				},
			},
		}
		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
		c.Assert(err, check.IsNil)

		rsp := s.asyncReq(c, req, nil, actionIsExpected)

		st.Lock()
		chg := st.Change(rsp.Change)
		tasks := chg.Tasks()
		st.Unlock()

		c.Check(chg, check.NotNil, check.Commentf("%v", tc))
		c.Check(tasks, check.HasLen, tc.expectedNumTasks, check.Commentf("%v", tc))
		c.Check(soon, check.Equals, 1)
	}
}

func (s *systemsSuite) TestSystemInstallActionErrorMissingVolumes(c *check.C) {
	s.daemon(c)

	for _, tc := range []struct {
		installStep string
		expectedErr string
	}{
		{"finish", `cannot finish install for "20191119": cannot finish install without volumes data (api)`},
		{"setup-storage-encryption", `cannot setup storage encryption for install from "20191119": cannot setup storage encryption without volumes data (api)`},
	} {
		body := map[string]any{
			"action": "install",
			"step":   tc.installStep,
			// note that "on-volumes" is missing which will
			// trigger a bug
		}
		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
		c.Assert(err, check.IsNil)

		rspe := s.errorReq(c, req, nil, actionIsExpected)
		c.Check(rspe.Error(), check.Equals, tc.expectedErr)
	}
}

func (s *systemsSuite) TestSystemInstallActionError(c *check.C) {
	s.daemon(c)

	body := map[string]string{
		"action": "install",
		"step":   "unknown-install-step",
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20191119", buf)
	c.Assert(err, check.IsNil)

	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rspe.Error(), check.Equals, `unsupported install step "unknown-install-step" (api)`)
}

func (s *systemsSuite) TestSystemActionCheckPassphrase(c *check.C) {
	s.daemon(c)
	s.mockHybridSystem()

	// just mock values for output matching
	const expectedEntropy = uint32(10)
	const expectedMinEntropy = uint32(20)
	const expectedOptimalEntropy = uint32(50)

	restore := daemon.MockDeviceValidatePassphrase(func(mode device.AuthMode, s string) (device.AuthQuality, error) {
		c.Check(mode, check.Equals, device.AuthModePassphrase)
		return device.AuthQuality{
			Entropy:        expectedEntropy,
			MinEntropy:     expectedMinEntropy,
			OptimalEntropy: expectedOptimalEntropy,
		}, nil
	})
	defer restore()

	restore = daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(dm *devicestate.DeviceManager, s string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
		return nil, nil, &install.EncryptionSupportInfo{PassphraseAuthAvailable: true}, nil
	})
	defer restore()

	body := map[string]string{
		"action":     "check-passphrase",
		"passphrase": "this is a good passphrase",
	}

	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20250619", buf)
	c.Assert(err, check.IsNil)

	rsp := s.syncReq(c, req, nil, actionIsExpected)
	c.Assert(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.DeepEquals, map[string]any{
		"entropy-bits":         uint32(10),
		"min-entropy-bits":     uint32(20),
		"optimal-entropy-bits": uint32(50),
	})
}

func (s *systemsSuite) TestSystemNoLabelInstallActionError(c *check.C) {
	s.daemon(c)

	body := map[string]string{
		"action": "install",
		"step":   "unknown-install-step",
	}
	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems", buf)
	c.Assert(err, check.IsNil)

	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(rspe.Error(), check.Equals, `unsupported install step "unknown-install-step" (api)`)
}

func (s *systemsSuite) TestSystemActionCheckPassphraseError(c *check.C) {
	d := s.daemon(c)
	s.mockHybridSystem()

	// just mock values for output matching
	const expectedEntropy = uint32(10)
	const expectedMinEntropy = uint32(20)
	const expectedOptimalEntropy = uint32(50)

	for _, tc := range []struct {
		passphrase  string
		noLabel     bool
		unavailable bool

		expectedStatus   int
		expectedErrKind  client.ErrorKind
		expectedErrMsg   string
		expectedErrValue any

		mockSupportErr error
	}{
		{
			noLabel:        true,
			expectedStatus: 400, expectedErrMsg: "system action requires the system label to be provided",
		},
		{
			passphrase:     "",
			expectedStatus: 400, expectedErrMsg: `passphrase must be provided in request body for action "check-passphrase"`,
		},
		{
			passphrase: "this is a good password", unavailable: true,
			expectedStatus: 400, expectedErrKind: "unsupported", expectedErrMsg: "target system does not support passphrase authentication",
		},
		{
			passphrase: "this is a good password", mockSupportErr: errors.New("mock error"),
			expectedStatus: 500, expectedErrMsg: "mock error",
		},
		{
			passphrase:     "bad-passphrase",
			expectedStatus: 400, expectedErrKind: "invalid-passphrase", expectedErrMsg: "passphrase did not pass quality checks",
			expectedErrValue: map[string]any{
				"reasons":              []device.AuthQualityErrorReason{device.AuthQualityErrorReasonLowEntropy},
				"entropy-bits":         expectedEntropy,
				"min-entropy-bits":     expectedMinEntropy,
				"optimal-entropy-bits": expectedOptimalEntropy,
			},
		},
	} {
		body := map[string]string{
			"action":     "check-passphrase",
			"passphrase": tc.passphrase,
		}

		route := "/v2/systems/20250122"
		if tc.noLabel {
			route = "/v2/systems"
		}

		restore := daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(dm *devicestate.DeviceManager, s string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
			return nil, nil, &install.EncryptionSupportInfo{PassphraseAuthAvailable: !tc.unavailable}, tc.mockSupportErr
		})
		defer restore()

		restore = daemon.MockDeviceValidatePassphrase(func(mode device.AuthMode, s string) (device.AuthQuality, error) {
			c.Check(mode, check.Equals, device.AuthModePassphrase)
			return device.AuthQuality{}, &device.AuthQualityError{
				Reasons: []device.AuthQualityErrorReason{device.AuthQualityErrorReasonLowEntropy},
				Quality: device.AuthQuality{
					Entropy:        expectedEntropy,
					MinEntropy:     expectedMinEntropy,
					OptimalEntropy: expectedOptimalEntropy,
				},
			}
		})
		defer restore()

		st := d.Overlord().State()
		st.Lock()
		daemon.ClearCachedEncryptionSupportInfoForLabel(d.Overlord().State(), "20250122")
		st.Unlock()

		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", route, buf)
		c.Assert(err, check.IsNil)

		rspe := s.errorReq(c, req, nil, actionExpectedBool(!tc.noLabel))
		c.Assert(rspe.Status, check.Equals, tc.expectedStatus)
		c.Assert(rspe.Kind, check.Equals, tc.expectedErrKind)
		c.Assert(rspe.Message, check.Matches, tc.expectedErrMsg)
		c.Assert(rspe.Value, check.DeepEquals, tc.expectedErrValue)
	}
}

func (s *systemsSuite) TestSystemActionCheckPIN(c *check.C) {
	s.daemon(c)
	s.mockHybridSystem()

	// just mock values for output matching
	const expectedEntropy = uint32(10)
	const expectedMinEntropy = uint32(20)
	const expectedOptimalEntropy = uint32(50)

	restore := daemon.MockDeviceValidatePassphrase(func(mode device.AuthMode, s string) (device.AuthQuality, error) {
		c.Check(mode, check.Equals, device.AuthModePIN)
		return device.AuthQuality{
			Entropy:        expectedEntropy,
			MinEntropy:     expectedMinEntropy,
			OptimalEntropy: expectedOptimalEntropy,
		}, nil
	})
	defer restore()

	restore = daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(dm *devicestate.DeviceManager, s string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
		return nil, nil, &install.EncryptionSupportInfo{PINAuthAvailable: true}, nil
	})
	defer restore()

	body := map[string]string{
		"action": "check-pin",
		"pin":    "20250619",
	}

	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(b)
	req, err := http.NewRequest("POST", "/v2/systems/20250619", buf)
	c.Assert(err, check.IsNil)

	rsp := s.syncReq(c, req, nil, actionIsExpected)
	c.Assert(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.DeepEquals, map[string]any{
		"entropy-bits":         uint32(10),
		"min-entropy-bits":     uint32(20),
		"optimal-entropy-bits": uint32(50),
	})
}

func (s *systemsSuite) TestSystemActionCheckPINError(c *check.C) {
	d := s.daemon(c)
	s.mockHybridSystem()

	// just mock values for output matching
	const expectedEntropy = uint32(10)
	const expectedMinEntropy = uint32(20)
	const expectedOptimalEntropy = uint32(50)

	for _, tc := range []struct {
		pin         string
		noLabel     bool
		unavailable bool

		expectedStatus   int
		expectedErrKind  client.ErrorKind
		expectedErrMsg   string
		expectedErrValue any

		mockSupportErr error
	}{
		{
			noLabel:        true,
			expectedStatus: 400, expectedErrMsg: "system action requires the system label to be provided",
		},
		{
			pin:            "",
			expectedStatus: 400, expectedErrMsg: `pin must be provided in request body for action "check-pin"`,
		},
		{
			pin: "123456", unavailable: true,
			expectedStatus: 400, expectedErrKind: "unsupported", expectedErrMsg: "target system does not support PIN authentication",
		},
		{
			pin: "123456", mockSupportErr: errors.New("mock error"),
			expectedStatus: 500, expectedErrMsg: "mock error",
		},
		{
			pin:            "0",
			expectedStatus: 400, expectedErrKind: "invalid-pin", expectedErrMsg: "PIN did not pass quality checks",
			expectedErrValue: map[string]any{
				"reasons":              []device.AuthQualityErrorReason{device.AuthQualityErrorReasonLowEntropy},
				"entropy-bits":         expectedEntropy,
				"min-entropy-bits":     expectedMinEntropy,
				"optimal-entropy-bits": expectedOptimalEntropy,
			},
		},
	} {
		body := map[string]string{
			"action": "check-pin",
			"pin":    tc.pin,
		}

		route := "/v2/systems/20250122"
		if tc.noLabel {
			route = "/v2/systems"
		}

		st := d.Overlord().State()

		restore := daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(dm *devicestate.DeviceManager, s string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
			// Lock and unlock to make sure caching logic doesn't go into deadlock with encryption info retrieval
			st.Lock()
			st.Unlock()
			return nil, nil, &install.EncryptionSupportInfo{PINAuthAvailable: !tc.unavailable}, tc.mockSupportErr
		})
		defer restore()

		restore = daemon.MockDeviceValidatePassphrase(func(mode device.AuthMode, s string) (device.AuthQuality, error) {
			c.Check(mode, check.Equals, device.AuthModePIN)
			return device.AuthQuality{}, &device.AuthQualityError{
				Reasons: []device.AuthQualityErrorReason{device.AuthQualityErrorReasonLowEntropy},
				Quality: device.AuthQuality{
					Entropy:        expectedEntropy,
					MinEntropy:     expectedMinEntropy,
					OptimalEntropy: expectedOptimalEntropy,
				},
			}
		})
		defer restore()

		st.Lock()
		daemon.ClearCachedEncryptionSupportInfoForLabel(d.Overlord().State(), "20250122")
		st.Unlock()

		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", route, buf)
		c.Assert(err, check.IsNil)

		rspe := s.errorReq(c, req, nil, actionExpectedBool(!tc.noLabel))
		c.Assert(rspe.Status, check.Equals, tc.expectedStatus)
		c.Assert(rspe.Kind, check.Equals, tc.expectedErrKind)
		c.Assert(rspe.Message, check.Matches, tc.expectedErrMsg)
		c.Assert(rspe.Value, check.DeepEquals, tc.expectedErrValue)
	}
}

func (s *systemsSuite) TestSystemActionCheckPassphraseOrPINCacheEncryptionInfo(c *check.C) {
	if !secboot.WithSecbootSupport {
		c.Skip("secboot is not available")
	}

	d := s.daemon(c)
	s.mockHybridSystem()

	called := 0
	restore := daemon.MockDeviceManagerSystemAndGadgetAndEncryptionInfo(func(dm *devicestate.DeviceManager, s string) (*devicestate.System, *gadget.Info, *install.EncryptionSupportInfo, error) {
		called++
		// Lock and unlock to make sure caching logic doesn't go into deadlock with encryption info retrieval
		st := d.Overlord().State()
		st.Lock()
		st.Unlock()
		return nil, nil, &install.EncryptionSupportInfo{PassphraseAuthAvailable: true, PINAuthAvailable: true}, nil
	})
	defer restore()

	body := map[string]string{
		"action":     "check-passphrase",
		"passphrase": "this is a good passphrase",
	}

	for i := 0; i < 10; i++ {
		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", "/v2/systems/20250122", buf)
		c.Assert(err, check.IsNil)

		rsp := s.syncReq(c, req, nil, actionIsExpected)
		c.Assert(rsp.Status, check.Equals, 200)
	}

	body = map[string]string{
		"action": "check-pin",
		"pin":    "20250619",
	}

	for i := 0; i < 10; i++ {
		b, err := json.Marshal(body)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(b)
		req, err := http.NewRequest("POST", "/v2/systems/20250122", buf)
		c.Assert(err, check.IsNil)

		rsp := s.syncReq(c, req, nil, actionIsExpected)
		c.Assert(rsp.Status, check.Equals, 200)
	}

	// Verify that encryption info calculation is called once and retrieved from cache otherwise.
	c.Check(called, check.Equals, 1)
}

var _ = check.Suite(&systemsCreateSuite{})

type systemsCreateSuite struct {
	apiBaseSuite

	storeSigning              *assertstest.StoreStack
	dev1Signing               *assertstest.SigningDB
	dev1acct                  *asserts.Account
	acct1Key                  *asserts.AccountKey
	mockSeqFormingAssertionFn func(assertType *asserts.AssertionType, sequenceKey []string, sequence int, user *auth.UserState) (asserts.Assertion, error)
	mockAssertionFn           func(at *asserts.AssertionType, headers []string, user *auth.UserState) (asserts.Assertion, error)
}

func (s *systemsCreateSuite) mockDevAssertion(c *check.C, t *asserts.AssertionType, extras map[string]any) asserts.Assertion {
	headers := map[string]any{
		"type":         t.Name,
		"authority-id": s.dev1acct.AccountID(),
		"account-id":   s.dev1acct.AccountID(),
		"series":       "16",
		"revision":     "5",
		"timestamp":    "2030-11-06T09:16:26Z",
	}

	for k, v := range extras {
		headers[k] = v
	}

	vs, err := s.dev1Signing.Sign(t, headers, nil, "")
	c.Assert(err, check.IsNil)
	return vs
}

func (s *systemsCreateSuite) mockStoreAssertion(c *check.C, t *asserts.AssertionType, extras map[string]any) asserts.Assertion {
	return mockStoreAssertion(c, s.storeSigning, s.storeSigning.AuthorityID, s.dev1acct.AccountID(), t, extras)
}

func mockStoreAssertion(
	c *check.C,
	signer assertstest.SignerDB,
	authorityID, accountID string,
	t *asserts.AssertionType,
	extras map[string]any,
) asserts.Assertion {
	headers := map[string]any{
		"type":         t.Name,
		"authority-id": authorityID,
		"account-id":   accountID,
		"series":       "16",
		"revision":     "5",
		"timestamp":    "2030-11-06T09:16:26Z",
	}

	for k, v := range extras {
		headers[k] = v
	}

	vs, err := signer.Sign(t, headers, nil, "")
	c.Assert(err, check.IsNil)
	return vs
}

func (s *systemsCreateSuite) SetUpTest(c *check.C) {
	s.apiBaseSuite.SetUpTest(c)
	d := s.daemon(c)

	s.expectRootAccess()

	restore := asserts.MockMaxSupportedFormat(asserts.ValidationSetType, 1)
	s.AddCleanup(restore)

	s.mockSeqFormingAssertionFn = nil
	s.mockAssertionFn = nil

	s.storeSigning = assertstest.NewStoreStack("can0nical", nil)

	st := d.Overlord().State()
	st.Lock()
	snapstate.ReplaceStore(st, s)
	assertstatetest.AddMany(st, s.storeSigning.StoreAccountKey(""))
	st.Unlock()

	s.dev1acct = assertstest.NewAccount(s.storeSigning, "developer1", nil, "")
	c.Assert(s.storeSigning.Add(s.dev1acct), check.IsNil)

	dev1PrivKey, _ := assertstest.GenerateKey(752)
	s.acct1Key = assertstest.NewAccountKey(s.storeSigning, s.dev1acct, nil, dev1PrivKey.PublicKey(), "")

	s.dev1Signing = assertstest.NewSigningDB(s.dev1acct.AccountID(), dev1PrivKey)
	c.Assert(s.storeSigning.Add(s.acct1Key), check.IsNil)

	d.Overlord().Loop()
	s.AddCleanup(func() { d.Overlord().Stop() })
}

func (s *systemsCreateSuite) Assertion(at *asserts.AssertionType, headers []string, user *auth.UserState) (asserts.Assertion, error) {
	s.pokeStateLock()
	return s.mockAssertionFn(at, headers, user)
}

func (s *systemsCreateSuite) SeqFormingAssertion(assertType *asserts.AssertionType, sequenceKey []string, sequence int, user *auth.UserState) (asserts.Assertion, error) {
	s.pokeStateLock()
	return s.mockSeqFormingAssertionFn(assertType, sequenceKey, sequence, user)
}

func (s *systemsCreateSuite) TestCreateSystemActionBadRequests(c *check.C) {
	type test struct {
		body       map[string]any
		routeLabel string
		result     string
	}

	tests := []test{
		{
			body: map[string]any{
				"action": "create",
			},
			routeLabel: "label",
			result:     `label should not be provided in route when creating a system \(api\)`,
		},
		{
			body: map[string]any{
				"action": "create",
				"label":  "",
			},
			result: `label must be provided in request body for action "create" \(api\)`,
		},
		{
			body: map[string]any{
				"action": "create",
				"label":  "label",
				"validation-sets": []string{
					"not-a-validation-set",
				},
			},
			result: `cannot parse validation sets: cannot parse validation set "not-a-validation-set": expected a single account/name \(api\)`,
		},
		{
			body: map[string]any{
				"action": "create",
				"label":  "label",
				"validation-sets": []string{
					"account/name",
				},
			},
			result: `cannot fetch validation sets: validation-set assertion not found \(api\)`,
		},
	}

	s.mockSeqFormingAssertionFn = func(assertType *asserts.AssertionType, sequenceKey []string, sequence int, user *auth.UserState) (asserts.Assertion, error) {
		return nil, &asserts.NotFoundError{
			Type: assertType,
		}
	}

	for _, tc := range tests {
		b, err := json.Marshal(tc.body)
		c.Assert(err, check.IsNil)

		url := "/v2/systems"
		if tc.routeLabel != "" {
			url += "/" + tc.routeLabel
		}

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(b))
		c.Assert(err, check.IsNil)

		rspe := s.errorReq(c, req, nil, actionIsExpected)
		c.Check(rspe.Status, check.Equals, 400)
		c.Check(rspe, check.ErrorMatches, tc.result, check.Commentf("%+v", tc))
	}
}

func (s *systemsCreateSuite) TestCreateSystemActionValidationSet(c *check.C) {
	const valSetSequence = 0
	s.testCreateSystemAction(c, valSetSequence)
}

func (s *systemsCreateSuite) TestCreateSystemActionSpecificValdationSet(c *check.C) {
	const valSetSequence = 1
	s.testCreateSystemAction(c, valSetSequence)
}

func (s *systemsCreateSuite) testCreateSystemAction(c *check.C, requestedValSetSequence int) {
	snaps := []any{
		map[string]any{
			"name":     "pc-kernel",
			"id":       snaptest.AssertedSnapID("pc-kernel"),
			"revision": "10",
			"presence": "required",
		},
		map[string]any{
			"name":     "pc",
			"id":       snaptest.AssertedSnapID("pc"),
			"revision": "10",
			"presence": "required",
		},
		map[string]any{
			"name":     "core20",
			"id":       snaptest.AssertedSnapID("core20"),
			"revision": "10",
			"presence": "required",
		},
	}

	accountID := s.dev1acct.AccountID()

	const validationSet = "validation-set-1"

	vsetAssert := s.mockDevAssertion(c, asserts.ValidationSetType, map[string]any{
		"name":     validationSet,
		"sequence": "1",
		"snaps":    snaps,
	})

	s.mockAssertionFn = func(at *asserts.AssertionType, key []string, user *auth.UserState) (asserts.Assertion, error) {
		headers, err := asserts.HeadersFromPrimaryKey(at, key)
		if err != nil {
			return nil, err
		}

		return s.storeSigning.Find(at, headers)
	}

	s.mockSeqFormingAssertionFn = func(assertType *asserts.AssertionType, sequenceKey []string, sequence int, user *auth.UserState) (asserts.Assertion, error) {
		if assertType != asserts.ValidationSetType {
			return nil, &asserts.NotFoundError{
				Type: assertType,
			}
		}

		c.Check(sequence, check.Equals, requestedValSetSequence)

		return vsetAssert, nil
	}

	const (
		markDefault   = true
		testSystem    = true
		expectedLabel = "1234"
	)

	daemon.MockDevicestateCreateRecoverySystem(func(st *state.State, label string, opts devicestate.CreateRecoverySystemOptions) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		c.Check(markDefault, check.Equals, opts.MarkDefault)
		c.Check(testSystem, check.Equals, opts.TestSystem)

		c.Check(opts.ValidationSets, check.HasLen, 1)

		for _, vs := range opts.ValidationSets {
			c.Check(vs.AccountID(), check.Equals, accountID)
		}

		return st.NewChange("change", "..."), nil
	})

	valSetString := accountID + "/" + validationSet
	if requestedValSetSequence > 0 {
		valSetString += "=" + strconv.Itoa(requestedValSetSequence)
	}

	body := map[string]any{
		"action":          "create",
		"label":           expectedLabel,
		"validation-sets": []string{valSetString},
		"mark-default":    markDefault,
		"test-system":     testSystem,
	}

	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/systems", bytes.NewBuffer(b))
	c.Assert(err, check.IsNil)

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st := s.d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}

func createFormData(c *check.C, fields map[string][]string, snaps map[string]string) (bytes.Buffer, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)

	for k, vs := range fields {
		for _, v := range vs {
			err := w.WriteField(k, v)
			c.Assert(err, check.IsNil)
		}
	}

	for name, content := range snaps {
		part, err := w.CreateFormFile("snap", name)
		c.Assert(err, check.IsNil)

		_, err = part.Write([]byte(content))
		c.Assert(err, check.IsNil)
	}

	err := w.Close()
	c.Assert(err, check.IsNil)

	return b, w.Boundary()
}

func (s *systemsCreateSuite) TestRemoveSystemAction(c *check.C) {
	const expectedLabel = "1234"

	daemon.MockDevicestateRemoveRecoverySystem(func(st *state.State, label string) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)

		return st.NewChange("change", "..."), nil
	})

	body := map[string]any{
		"action": "remove",
	}

	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/systems/"+expectedLabel, bytes.NewBuffer(b))
	c.Assert(err, check.IsNil)

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st := s.d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}

func (s *systemsCreateSuite) TestRemoveSystemActionNotFound(c *check.C) {
	const expectedLabel = "1234"

	daemon.MockDevicestateRemoveRecoverySystem(func(st *state.State, label string) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		return nil, devicestate.ErrNoRecoverySystem
	})

	body := map[string]any{
		"action": "remove",
	}

	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/systems/"+expectedLabel, bytes.NewBuffer(b))
	c.Assert(err, check.IsNil)

	res := s.errorReq(c, req, nil, actionIsExpected)
	c.Check(res.Status, check.Equals, 404)
	c.Check(res.Message, check.Equals, "recovery system does not exist")
}

func (s *systemsCreateSuite) TestCreateSystemActionOfflineBadRequests(c *check.C) {
	type test struct {
		fields map[string][]string
		result string
	}

	tests := []test{
		{
			fields: map[string][]string{
				"action": {"create"},
				"label":  {"1", "2"},
			},
			result: `expected exactly one "label" value in form \(api\)`,
		},
		{
			fields: map[string][]string{
				"action":      {"create"},
				"label":       {"1"},
				"test-system": {"false", "true"},
			},
			result: `expected at most one "test-system" value in form \(api\)`,
		},
		{
			fields: map[string][]string{
				"action":       {"create"},
				"label":        {"1"},
				"mark-default": {"false", "true"},
			},
			result: `expected at most one "mark-default" value in form \(api\)`,
		},
		{
			fields: map[string][]string{
				"action":          {"create"},
				"label":           {"1"},
				"validation-sets": {"id/set-1", "id/set-2"},
			},
			result: `expected at most one "validation-sets" value in form \(api\)`,
		},
		{
			fields: map[string][]string{
				"action":      {"create"},
				"label":       {"1"},
				"test-system": {"not-valid"},
			},
			result: `cannot parse "test-system" value as boolean: not-valid \(api\)`,
		},
		{
			fields: map[string][]string{
				"action":       {"create"},
				"label":        {"1"},
				"mark-default": {"not-valid"},
			},
			result: `cannot parse "mark-default" value as boolean: not-valid \(api\)`,
		},
		{
			fields: map[string][]string{
				"action":          {"create"},
				"label":           {"1"},
				"validation-sets": {"invalid-set-name"},
			},
			result: `cannot parse validation sets: cannot parse validation set "invalid-set-name": expected a single account/name \(api\)`,
		},
	}

	snaps := map[string]string{
		"snap-1": "snap-1 contents",
		"snap-2": "snap-2 contents",
	}

	for _, tc := range tests {
		form, boundary := createFormData(c, tc.fields, snaps)

		req, err := http.NewRequest("POST", "/v2/systems", &form)
		c.Assert(err, check.IsNil)
		req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
		req.Header.Set("Content-Length", strconv.Itoa(form.Len()))

		rspe := s.errorReq(c, req, nil, actionIsExpected)
		c.Check(rspe.Status, check.Equals, 400)
		c.Check(rspe, check.ErrorMatches, tc.result, check.Commentf("%+v", tc))

		// make sure that form files we uploaded get removed on failure
		files, err := filepath.Glob(filepath.Join(dirs.SnapBlobDir, dirs.LocalInstallBlobTempPrefix+"*"))
		c.Assert(err, check.IsNil)
		c.Check(files, check.HasLen, 0)
	}
}

func (s *systemsCreateSuite) TestCreateSystemActionOffline(c *check.C) {
	snaps := []any{
		map[string]any{
			"name":     "pc-kernel",
			"id":       snaptest.AssertedSnapID("pc-kernel"),
			"revision": "10",
			"presence": "required",
		},
		map[string]any{
			"name":     "pc",
			"id":       snaptest.AssertedSnapID("pc"),
			"revision": "10",
			"presence": "required",
		},
		map[string]any{
			"name":     "core20",
			"id":       snaptest.AssertedSnapID("core20"),
			"revision": "10",
			"presence": "required",
		},
	}

	accountID := s.dev1acct.AccountID()

	const (
		validationSet = "validation-set-1"
		expectedLabel = "1234"
	)

	vsetAssert := s.mockDevAssertion(c, asserts.ValidationSetType, map[string]any{
		"name":     validationSet,
		"sequence": "1",
		"snaps":    snaps,
	})

	assertions := []string{
		string(asserts.Encode(vsetAssert)),
		string(asserts.Encode(s.acct1Key)),
		string(asserts.Encode(s.dev1acct)),
	}

	snapFormData := make(map[string]string)
	for _, name := range []string{"pc-kernel", "pc", "core20"} {
		f := snaptest.MakeTestSnapWithFiles(c, fmt.Sprintf("name: %s\nversion: 1", name), nil)
		digest, size, err := asserts.SnapFileSHA3_384(f)
		c.Assert(err, check.IsNil)

		rev := s.mockStoreAssertion(c, asserts.SnapRevisionType, map[string]any{
			"snap-id":       snaptest.AssertedSnapID(name),
			"snap-sha3-384": digest,
			"developer-id":  s.dev1acct.AccountID(),
			"snap-size":     strconv.Itoa(int(size)),
			"snap-revision": "10",
		})

		// this is required right now. should it be?
		decl := s.mockStoreAssertion(c, asserts.SnapDeclarationType, map[string]any{
			"series":       "16",
			"snap-id":      snaptest.AssertedSnapID(name),
			"snap-name":    name,
			"publisher-id": s.dev1acct.AccountID(),
			"timestamp":    time.Now().Format(time.RFC3339),
		})

		assertions = append(assertions, string(asserts.Encode(rev)), string(asserts.Encode(decl)))

		content, err := os.ReadFile(f)
		c.Assert(err, check.IsNil)

		snapFormData[name] = string(content)
	}

	valSetString := accountID + "/" + validationSet
	fields := map[string][]string{
		"action":          {"create"},
		"assertion":       assertions,
		"label":           {expectedLabel},
		"validation-sets": {valSetString},
	}

	form, boundary := createFormData(c, fields, snapFormData)

	daemon.MockDevicestateCreateRecoverySystem(func(st *state.State, label string, opts devicestate.CreateRecoverySystemOptions) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		c.Check(opts.ValidationSets, check.HasLen, 1)
		c.Check(opts.ValidationSets[0].Body(), check.DeepEquals, vsetAssert.Body())

		c.Check(opts.LocalSnaps, check.HasLen, 3)

		for _, vs := range opts.ValidationSets {
			c.Check(vs.AccountID(), check.Equals, accountID)
		}

		return st.NewChange("change", "..."), nil
	})

	req, err := http.NewRequest("POST", "/v2/systems", &form)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(form.Len()))

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st := s.d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}

func (s *systemsCreateSuite) TestCreateSystemActionWithComponentsOffline(c *check.C) {
	snaps := []any{
		map[string]any{
			"name":     "pc-kernel",
			"id":       snaptest.AssertedSnapID("pc-kernel"),
			"revision": "10",
			"presence": "required",
			"components": map[string]any{
				"kmod": map[string]any{
					"revision": "10",
					"presence": "required",
				},
			},
		},
		map[string]any{
			"name":     "pc",
			"id":       snaptest.AssertedSnapID("pc"),
			"revision": "10",
			"presence": "required",
		},
		map[string]any{
			"name":     "core20",
			"id":       snaptest.AssertedSnapID("core20"),
			"revision": "10",
			"presence": "required",
		},
	}

	accountID := s.dev1acct.AccountID()

	const (
		validationSet = "validation-set-1"
		expectedLabel = "1234"
	)

	vsetAssert := s.mockDevAssertion(c, asserts.ValidationSetType, map[string]any{
		"name":     validationSet,
		"sequence": "1",
		"snaps":    snaps,
	})

	assertions := []string{
		string(asserts.Encode(vsetAssert)),
		string(asserts.Encode(s.acct1Key)),
		string(asserts.Encode(s.dev1acct)),
	}

	snapComponents := map[string][]string{
		"pc-kernel":  {"kmod"},
		"extra-snap": {"snap-1"},
	}

	snapFormData := make(map[string]string)

	st := s.d.Overlord().State()
	for _, name := range []string{"pc-kernel", "pc", "core20", "extra-snap"} {
		f := snaptest.MakeTestSnapWithFiles(c, withComponents(fmt.Sprintf("name: %s\nversion: 1", name), snapComponents[name]), nil)
		digest, size, err := asserts.SnapFileSHA3_384(f)
		c.Assert(err, check.IsNil)

		snapID := snaptest.AssertedSnapID(name)

		rev := s.mockStoreAssertion(c, asserts.SnapRevisionType, map[string]any{
			"snap-id":       snapID,
			"snap-sha3-384": digest,
			"developer-id":  s.dev1acct.AccountID(),
			"snap-size":     strconv.Itoa(int(size)),
			"snap-revision": "10",
		})

		decl := s.mockStoreAssertion(c, asserts.SnapDeclarationType, map[string]any{
			"series":       "16",
			"snap-id":      snapID,
			"snap-name":    name,
			"publisher-id": s.dev1acct.AccountID(),
			"timestamp":    time.Now().Format(time.RFC3339),
		})

		assertions = append(assertions, string(asserts.Encode(rev)), string(asserts.Encode(decl)))

		for _, comp := range snapComponents[name] {
			compPath, resRev, resPair := s.makeStandardComponent(c, name, comp)

			assertions = append(assertions, string(asserts.Encode(resRev)), string(asserts.Encode(resPair)))

			content, err := os.ReadFile(compPath)
			c.Assert(err, check.IsNil)

			snapFormData[fmt.Sprintf("%s+%s.comp", name, comp)] = string(content)
		}

		content, err := os.ReadFile(f)
		c.Assert(err, check.IsNil)

		// we exclude this snap from being uploaded since we want to test
		// uploading a component without its associated snap
		if name != "extra-snap" {
			snapFormData[name] = string(content)
		} else {
			st.Lock()
			snapstate.Set(st, name, &snapstate.SnapState{
				Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
					{
						RealName: name,
						Revision: snap.R(10),
						SnapID:   snapID,
					},
				}),
				Current: snap.R(10),
				Active:  true,
			})
			st.Unlock()
		}
	}

	valSetString := accountID + "/" + validationSet
	fields := map[string][]string{
		"action":          {"create"},
		"assertion":       assertions,
		"label":           {expectedLabel},
		"validation-sets": {valSetString},
	}

	form, boundary := createFormData(c, fields, snapFormData)

	daemon.MockDevicestateCreateRecoverySystem(func(st *state.State, label string, opts devicestate.CreateRecoverySystemOptions) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		c.Check(opts.ValidationSets, check.HasLen, 1)
		c.Check(opts.ValidationSets[0].Body(), check.DeepEquals, vsetAssert.Body())
		c.Check(opts.LocalSnaps, check.HasLen, 3)
		c.Check(opts.LocalComponents, check.HasLen, 2)

		for _, vs := range opts.ValidationSets {
			c.Check(vs.AccountID(), check.Equals, accountID)
		}

		return st.NewChange("change", "..."), nil
	})

	req, err := http.NewRequest("POST", "/v2/systems", &form)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(form.Len()))

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}

func (s *systemsCreateSuite) makeStandardComponent(c *check.C, snapName string, compName string) (compPath string, resourceRev, resourcePair asserts.Assertion) {
	return makeStandardComponent(c, s.storeSigning, s.storeSigning.AuthorityID, s.dev1acct.AccountID(), snapName, compName)
}

func makeStandardComponent(
	c *check.C,
	signer assertstest.SignerDB,
	authorityID string,
	accountID string,
	snapName string,
	compName string,
) (compPath string, resourceRev, resourcePair asserts.Assertion) {
	yaml := fmt.Sprintf("component: %s+%s\nversion: 1\ntype: standard", snapName, compName)
	compPath = snaptest.MakeTestComponent(c, yaml)

	digest, size, err := asserts.SnapFileSHA3_384(compPath)
	c.Assert(err, check.IsNil)

	resRev := mockStoreAssertion(c, signer, authorityID, accountID, asserts.SnapResourceRevisionType, map[string]any{
		"snap-id":           snaptest.AssertedSnapID(snapName),
		"developer-id":      accountID,
		"resource-name":     compName,
		"resource-sha3-384": digest,
		"resource-revision": "20",
		"resource-size":     strconv.Itoa(int(size)),
		"timestamp":         time.Now().Format(time.RFC3339),
	})

	resPair := mockStoreAssertion(c, signer, authorityID, accountID, asserts.SnapResourcePairType, map[string]any{
		"snap-id":           snaptest.AssertedSnapID(snapName),
		"developer-id":      accountID,
		"resource-name":     compName,
		"resource-revision": "20",
		"snap-revision":     "10",
		"timestamp":         time.Now().UTC().Format(time.RFC3339),
	})

	return compPath, resRev, resPair
}

func (s *systemsCreateSuite) TestCreateSystemActionOfflinePreinstalledJSON(c *check.C) {
	const (
		expectedLabel = "1234"
	)

	daemon.MockDevicestateCreateRecoverySystem(func(st *state.State, label string, opts devicestate.CreateRecoverySystemOptions) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		c.Check(opts.ValidationSets, check.HasLen, 0)
		c.Check(opts.LocalSnaps, check.HasLen, 0)
		c.Check(opts.Offline, check.Equals, true)

		return st.NewChange("change", "..."), nil
	})

	body := map[string]any{
		"action":  "create",
		"label":   expectedLabel,
		"offline": true,
	}

	b, err := json.Marshal(body)
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/systems", bytes.NewBuffer(b))
	c.Assert(err, check.IsNil)

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st := s.d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}

func (s *systemsCreateSuite) TestCreateSystemActionOfflinePreinstalledForm(c *check.C) {
	const (
		expectedLabel = "1234"
	)

	fields := map[string][]string{
		"action": {"create"},
		"label":  {expectedLabel},
	}

	form, boundary := createFormData(c, fields, nil)

	daemon.MockDevicestateCreateRecoverySystem(func(st *state.State, label string, opts devicestate.CreateRecoverySystemOptions) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		c.Check(opts.ValidationSets, check.HasLen, 0)
		c.Check(opts.LocalSnaps, check.HasLen, 0)
		c.Check(opts.Offline, check.Equals, true)

		return st.NewChange("change", "..."), nil
	})

	req, err := http.NewRequest("POST", "/v2/systems", &form)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(form.Len()))

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st := s.d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}

func (s *systemsCreateSuite) TestCreateSystemActionOfflineJustValidationSets(c *check.C) {
	accountID := s.dev1acct.AccountID()

	const (
		validationSet = "validation-set-1"
		expectedLabel = "1234"
	)

	vsetAssert := s.mockDevAssertion(c, asserts.ValidationSetType, map[string]any{
		"name":     validationSet,
		"sequence": "1",
		"snaps": []any{
			map[string]any{
				"name":     "pc-kernel",
				"id":       snaptest.AssertedSnapID("pc-kernel"),
				"revision": "10",
				"presence": "required",
			},
			map[string]any{
				"name":     "pc",
				"id":       snaptest.AssertedSnapID("pc"),
				"revision": "10",
				"presence": "required",
			},
			map[string]any{
				"name":     "core20",
				"id":       snaptest.AssertedSnapID("core20"),
				"revision": "10",
				"presence": "required",
			},
		},
	})

	assertions := []string{
		string(asserts.Encode(vsetAssert)),
		string(asserts.Encode(s.acct1Key)),
		string(asserts.Encode(s.dev1acct)),
	}

	valSetString := accountID + "/" + validationSet
	fields := map[string][]string{
		"action":          {"create"},
		"assertion":       assertions,
		"label":           {expectedLabel},
		"validation-sets": {valSetString},
	}

	form, boundary := createFormData(c, fields, nil)

	daemon.MockDevicestateCreateRecoverySystem(func(st *state.State, label string, opts devicestate.CreateRecoverySystemOptions) (*state.Change, error) {
		c.Check(expectedLabel, check.Equals, label)
		c.Check(opts.ValidationSets, check.HasLen, 1)
		c.Check(opts.ValidationSets[0].Body(), check.DeepEquals, vsetAssert.Body())
		c.Check(opts.LocalSnaps, check.HasLen, 0)
		c.Check(opts.Offline, check.Equals, true)

		return st.NewChange("change", "..."), nil
	})

	req, err := http.NewRequest("POST", "/v2/systems", &form)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(form.Len()))

	res := s.asyncReq(c, req, nil, actionIsExpected)

	st := s.d.Overlord().State()
	st.Lock()
	defer st.Unlock()

	c.Check(st.Change(res.Change), check.NotNil)
}
