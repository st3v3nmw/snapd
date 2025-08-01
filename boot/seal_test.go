// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2020 Canonical Ltd
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

package boot_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/arch/archtest"
	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/boot/boottest"
	"github.com/snapcore/snapd/bootloader"
	"github.com/snapcore/snapd/bootloader/assets"
	"github.com/snapcore/snapd/bootloader/bootloadertest"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/gadget/device"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/secboot"
	"github.com/snapcore/snapd/seed"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/testutil"
	"github.com/snapcore/snapd/timings"
)

type sealSuite struct {
	testutil.BaseTest
}

var _ = Suite(&sealSuite{})

func (s *sealSuite) SetUpTest(c *C) {
	s.BaseTest.SetUpTest(c)

	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	s.AddCleanup(func() { dirs.SetRootDir("/") })
	s.AddCleanup(archtest.MockArchitecture("amd64"))
	snippets := []assets.ForEditions{
		{FirstEdition: 1, Snippet: []byte("console=ttyS0 console=tty1 panic=-1")},
	}
	s.AddCleanup(assets.MockSnippetsForEdition("grub.cfg:static-cmdline", snippets))
	s.AddCleanup(assets.MockSnippetsForEdition("grub-recovery.cfg:static-cmdline", snippets))
}

func mockKernelSeedSnap(rev snap.Revision) *seed.Snap {
	return boottest.MockNamedKernelSeedSnap(rev, "pc-kernel")
}

func mockGadgetSeedSnap(c *C, files [][]string) *seed.Snap {
	return boottest.MockGadgetSeedSnap(c, gadgetSnapYaml, files)
}

func (s *sealSuite) TestSealKeyToModeenv(c *C) {
	defer boot.MockSealModeenvLocked()()

	for idx, tc := range []struct {
		sealErr       error
		provisionErr  error
		factoryReset  bool
		shimId        string
		grubId        string
		runGrubId     string
		expErr        string
		expSealCalls  int
		disableTokens bool
	}{
		{
			expSealCalls: 1,
		},
		{
			expSealCalls:  1,
			disableTokens: true,
		}, {
			// old boot assets
			shimId: "bootx64.efi", grubId: "grubx64.efi",
			expSealCalls: 1,
		}, {
			factoryReset: true,
			expSealCalls: 1,
		}, {
			sealErr: errors.New("seal error"), expErr: `seal error`,
			expSealCalls: 1,
		},
	} {
		c.Logf("tc %v", idx)
		rootdir := c.MkDir()
		dirs.SetRootDir(rootdir)
		defer dirs.SetRootDir("")

		shimId := tc.shimId
		if shimId == "" {
			shimId = "ubuntu:shimx64.efi"
		}
		grubId := tc.grubId
		if grubId == "" {
			grubId = "ubuntu:grubx64.efi"
		}
		runGrubId := tc.runGrubId
		if runGrubId == "" {
			runGrubId = "grubx64.efi"
		}

		err := createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-seed"))
		c.Assert(err, IsNil)

		err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-boot"))
		c.Assert(err, IsNil)

		model := boottest.MakeMockUC20Model()

		modeenv := &boot.Modeenv{
			RecoverySystem: "20200825",
			CurrentTrustedRecoveryBootAssets: boot.BootAssetsMap{
				grubId: []string{"grub-hash-1"},
				shimId: []string{"shim-hash-1"},
			},

			CurrentTrustedBootAssets: boot.BootAssetsMap{
				runGrubId: []string{"run-grub-hash-1"},
			},

			CurrentKernels: []string{"pc-kernel_500.snap"},

			CurrentKernelCommandLines: boot.BootCommandLines{
				"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
			},
			Model:          model.Model(),
			BrandID:        model.BrandID(),
			Grade:          string(model.Grade()),
			ModelSignKeyID: model.SignKeyID(),
		}

		// mock asset cache
		boottest.MockAssetsCache(c, rootdir, "grub", []string{
			fmt.Sprintf("%s-shim-hash-1", shimId),
			fmt.Sprintf("%s-grub-hash-1", grubId),
			fmt.Sprintf("%s-run-grub-hash-1", runGrubId),
		})

		// set encryption key
		myKey := secboot.CreateMockBootstrappedContainer()
		myKey2 := secboot.CreateMockBootstrappedContainer()
		// and volumes authentication options
		myVolumesAuth := &device.VolumesAuthOptions{Mode: device.AuthModePassphrase, Passphrase: "test"}

		// set a mock recovery kernel
		readSystemEssentialCalls := 0
		restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
			readSystemEssentialCalls++
			return model, []*seed.Snap{mockKernelSeedSnap(snap.R(1)), mockGadgetSeedSnap(c, nil)}, nil
		})
		defer restore()

		sealKeyForBootChainsCalled := 0
		restore = boot.MockSealKeyForBootChains(func(method device.SealingMethod, key, saveKey secboot.BootstrappedContainer, primaryKey []byte, volumesAuth *device.VolumesAuthOptions, params *boot.SealKeyForBootChainsParams) error {
			sealKeyForBootChainsCalled++

			for _, d := range []string{boot.InitramfsSeedEncryptionKeyDir, filepath.Join(dirs.GlobalRootDir, "/run/mnt/ubuntu-data/system-data/var/lib/snapd/device/fde")} {
				ex, isdir, _ := osutil.DirExists(d)
				c.Check(ex && isdir, Equals, true, Commentf("location %q does not exist or is not a directory", d))
			}

			c.Check(method, Equals, device.SealingMethodTPM)
			c.Check(key, DeepEquals, myKey)
			c.Check(saveKey, DeepEquals, myKey2)
			c.Check(volumesAuth, Equals, myVolumesAuth)

			recoveryBootLoader, hasRecovery := params.RoleToBlName[bootloader.RoleRecovery]
			c.Assert(hasRecovery, Equals, true)
			c.Check(recoveryBootLoader, Equals, "grub")
			runBootLoader, hasRun := params.RoleToBlName[bootloader.RoleRunMode]
			c.Assert(hasRun, Equals, true)
			c.Check(runBootLoader, Equals, "grub")

			c.Assert(params.RunModeBootChains, HasLen, 1)
			runBootChain := params.RunModeBootChains[0]
			c.Check(runBootChain.Model, Equals, model.Model())
			c.Assert(runBootChain.AssetChain, HasLen, 3)
			runShim := runBootChain.AssetChain[0]
			runGrub := runBootChain.AssetChain[1]
			runGrubRun := runBootChain.AssetChain[2]
			c.Check(runShim.Name, Equals, shimId)
			c.Assert(runShim.Hashes, HasLen, 1)
			c.Check(runShim.Hashes[0], Equals, "shim-hash-1")
			c.Check(runGrub.Name, Equals, grubId)
			c.Assert(runGrub.Hashes, HasLen, 1)
			c.Check(runGrub.Hashes[0], Equals, "grub-hash-1")
			c.Check(runGrubRun.Name, Equals, runGrubId)
			c.Assert(runGrubRun.Hashes, HasLen, 1)
			c.Check(runGrubRun.Hashes[0], Equals, "run-grub-hash-1")

			c.Check(params.RecoveryBootChainsForRunKey, HasLen, 0)

			c.Assert(params.RecoveryBootChains, HasLen, 1)
			recoveryBootChain := params.RecoveryBootChains[0]
			c.Check(recoveryBootChain.Model, Equals, model.Model())
			c.Assert(recoveryBootChain.AssetChain, HasLen, 2)
			recoveryShim := recoveryBootChain.AssetChain[0]
			recoveryGrub := recoveryBootChain.AssetChain[1]
			c.Check(recoveryShim.Name, Equals, shimId)
			c.Assert(recoveryShim.Hashes, HasLen, 1)
			c.Check(recoveryShim.Hashes[0], Equals, "shim-hash-1")
			c.Check(recoveryGrub.Name, Equals, grubId)
			c.Assert(recoveryGrub.Hashes, HasLen, 1)
			c.Check(recoveryGrub.Hashes[0], Equals, "grub-hash-1")

			c.Check(params.FactoryReset, Equals, tc.factoryReset)
			c.Check(params.InstallHostWritableDir, Equals, filepath.Join(boot.InitramfsRunMntDir, "ubuntu-data", "system-data"))
			c.Check(params.UseTokens, Equals, !tc.disableTokens)

			return tc.sealErr
		})
		defer restore()

		u := mockUnlocker{}
		err = boot.SealKeyToModeenv(myKey, myKey2, nil, myVolumesAuth, model, modeenv, boot.MockSealKeyToModeenvFlags{
			FactoryReset:  tc.factoryReset,
			StateUnlocker: u.unlocker,
			UseTokens:     !tc.disableTokens,
		})
		c.Check(u.unlocked, Equals, 1)
		c.Check(sealKeyForBootChainsCalled, Equals, tc.expSealCalls)
		if tc.expErr == "" {
			c.Assert(err, IsNil)
		} else {
			c.Assert(err, ErrorMatches, tc.expErr)
			continue
		}
	}
}

type mockUnlocker struct {
	unlocked int
}

func (u *mockUnlocker) unlocker() func() {
	return func() {
		u.unlocked += 1
	}
}

// TODO:UC20: also test fallback reseal
func (s *sealSuite) TestResealKeyToModeenvWithSystemFallback(c *C) {
	defer boot.MockModeenvLocked()()

	for idx, tc := range []struct {
		sealedKeys bool
		resealErr  error
		shimId     string
		shimId2    string
		noShim2    bool
		grubId     string
		grubId2    string
		noGrub2    bool
		runGrubId  string
		err        string
	}{
		{sealedKeys: false, shimId: "bootx64.efi", grubId: "grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: true, shimId: "bootx64.efi", grubId: "grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: false, shimId: "bootx64.efi", grubId: "grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: true, shimId: "bootx64.efi", grubId: "grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: false, shimId2: "bootx64.efi", grubId2: "grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: true, shimId2: "bootx64.efi", grubId2: "grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: false, shimId: "bootx64.efi", grubId: "grubx64.efi", shimId2: "ubuntu:shimx64.efi", grubId2: "ubuntu:grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: true, shimId: "bootx64.efi", grubId: "grubx64.efi", shimId2: "ubuntu:shimx64.efi", grubId2: "ubuntu:grubx64.efi", resealErr: nil, err: ""},
		{sealedKeys: false, noGrub2: true, resealErr: nil, err: ""},
		{sealedKeys: true, noGrub2: true, resealErr: nil, err: ""},
		{sealedKeys: false, noShim2: true, resealErr: nil, err: ""},
		{sealedKeys: true, noShim2: true, resealErr: nil, err: ""},
		{sealedKeys: false, noShim2: true, noGrub2: true, resealErr: nil, err: ""},
		{sealedKeys: true, noShim2: true, noGrub2: true, resealErr: nil, err: ""},
		{sealedKeys: false, resealErr: nil, err: ""},
		{sealedKeys: true, resealErr: nil, err: ""},
		{sealedKeys: true, resealErr: errors.New("reseal error"), err: "reseal error"},
	} {
		c.Logf("tc: %v", idx)
		rootdir := c.MkDir()
		dirs.SetRootDir(rootdir)
		defer dirs.SetRootDir("")

		shimId := tc.shimId
		if shimId == "" {
			shimId = "ubuntu:shimx64.efi"
		}
		shimId2 := tc.shimId2
		if shimId2 == "" && !tc.noShim2 {
			shimId2 = shimId
		}
		grubId := tc.grubId
		if grubId == "" {
			grubId = "ubuntu:grubx64.efi"
		}
		grubId2 := tc.grubId2
		if grubId2 == "" && !tc.noGrub2 {
			grubId2 = grubId
		}
		runGrubId := tc.runGrubId
		if runGrubId == "" {
			runGrubId = "grubx64.efi"
		}

		if tc.sealedKeys {
			c.Assert(os.MkdirAll(dirs.SnapFDEDir, 0755), IsNil)
			err := os.WriteFile(filepath.Join(dirs.SnapFDEDir, "sealed-keys"), []byte(device.SealingMethodTPM), 0644)
			c.Assert(err, IsNil)
		}

		err := createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-seed"))
		c.Assert(err, IsNil)

		err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-boot"))
		c.Assert(err, IsNil)

		model := boottest.MakeMockUC20Model()

		recoveryBootAssets := boot.BootAssetsMap{}
		recoveryBootAssets[shimId] = append(recoveryBootAssets[shimId], "shim-hash-1")
		if shimId2 != "" {
			recoveryBootAssets[shimId2] = append(recoveryBootAssets[shimId2], "shim-hash-2")
		}
		recoveryBootAssets[grubId] = append(recoveryBootAssets[grubId], "grub-hash-1")
		if grubId2 != "" {
			recoveryBootAssets[grubId2] = append(recoveryBootAssets[grubId2], "grub-hash-2")
		}

		modeenv := &boot.Modeenv{
			CurrentRecoverySystems:           []string{"20200825"},
			CurrentTrustedRecoveryBootAssets: recoveryBootAssets,
			CurrentTrustedBootAssets: boot.BootAssetsMap{
				runGrubId: []string{"run-grub-hash-1", "run-grub-hash-2"},
			},

			CurrentKernels: []string{"pc-kernel_500.snap", "pc-kernel_600.snap"},

			CurrentKernelCommandLines: boot.BootCommandLines{
				"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
			},
			Model:          model.Model(),
			BrandID:        model.BrandID(),
			Grade:          string(model.Grade()),
			ModelSignKeyID: model.SignKeyID(),
		}

		// set a mock recovery kernel
		readSystemEssentialCalls := 0
		restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
			readSystemEssentialCalls++
			return model, []*seed.Snap{mockKernelSeedSnap(snap.R(1)), mockGadgetSeedSnap(c, nil)}, nil
		})
		defer restore()

		kernel := bootloader.NewBootFile(filepath.Join("/var/lib/snapd/seed/snaps/pc-kernel_1.snap"), "kernel.efi", bootloader.RoleRecovery)
		runKernel := bootloader.NewBootFile(filepath.Join(rootdir, "var/lib/snapd/snaps/pc-kernel_500.snap"), "kernel.efi", bootloader.RoleRunMode)
		runKernel2 := bootloader.NewBootFile(filepath.Join(rootdir, "var/lib/snapd/snaps/pc-kernel_600.snap"), "kernel.efi", bootloader.RoleRunMode)

		var expectedRecoveryBootChains []boot.BootChain
		var expectedRunBootChains []boot.BootChain
		var shimHashes []string
		shimHashes = append(shimHashes, "shim-hash-1")
		if shimId2 != "" && shimId2 == shimId {
			shimHashes = append(shimHashes, "shim-hash-2")
		}
		var grubHashes []string
		grubHashes = append(grubHashes, "grub-hash-1")
		if grubId2 != "" && grubId2 == grubId {
			grubHashes = append(grubHashes, "grub-hash-2")
		}
		// recovery boot chains
		expectedRecoveryBootChains = append(expectedRecoveryBootChains,
			boot.BootChain{
				BrandID:        "my-brand",
				Model:          "my-model-uc20",
				Grade:          "dangerous",
				ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
				AssetChain: []boot.BootAsset{
					{
						Role:   "recovery",
						Name:   shimId,
						Hashes: shimHashes,
					},
					{
						Role:   "recovery",
						Name:   grubId,
						Hashes: grubHashes,
					},
				},
				Kernel:         "pc-kernel",
				KernelRevision: "1",
				KernelCmdlines: []string{
					"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				},
				KernelBootFile: kernel,
			},
		)
		if shimId2 != "" && shimId2 != shimId && grubId2 != "" && grubId2 != grubId {
			expectedExtraRecoveryBootChains := []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain: []boot.BootAsset{
						{
							Role:   "recovery",
							Name:   shimId2,
							Hashes: []string{"shim-hash-2"},
						},
						{
							Role:   "recovery",
							Name:   grubId2,
							Hashes: []string{"grub-hash-2"},
						},
					},
					Kernel:         "pc-kernel",
					KernelRevision: "1",
					KernelCmdlines: []string{
						"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: kernel,
				},
			}
			if shimId == "bootx64.efi" {
				expectedRecoveryBootChains = append(expectedExtraRecoveryBootChains, expectedRecoveryBootChains...)
			} else {
				expectedRecoveryBootChains = append(expectedRecoveryBootChains, expectedExtraRecoveryBootChains...)
			}
		}

		for _, k := range []struct {
			Revision string
			BootFile bootloader.BootFile
		}{
			{Revision: "500", BootFile: runKernel},
			{Revision: "600", BootFile: runKernel2},
		} {
			expectedRunBootChainsForKernel := []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain: []boot.BootAsset{
						{
							Role:   "recovery",
							Name:   shimId,
							Hashes: shimHashes,
						},
						{
							Role:   "recovery",
							Name:   grubId,
							Hashes: grubHashes,
						},
						{
							Role:   "run-mode",
							Name:   runGrubId,
							Hashes: []string{"run-grub-hash-1", "run-grub-hash-2"},
						},
					},
					Kernel:         "pc-kernel",
					KernelRevision: k.Revision,
					KernelCmdlines: []string{
						"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: k.BootFile,
				},
			}
			if shimId2 != "" && shimId2 != shimId && grubId2 != "" && grubId2 != grubId {
				expectedExtraBootChains := []boot.BootChain{
					{
						BrandID:        "my-brand",
						Model:          "my-model-uc20",
						Grade:          "dangerous",
						ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
						AssetChain: []boot.BootAsset{
							{
								Role:   "recovery",
								Name:   shimId2,
								Hashes: []string{"shim-hash-2"},
							},
							{
								Role:   "recovery",
								Name:   grubId2,
								Hashes: []string{"grub-hash-2"},
							},
							{
								Role:   "run-mode",
								Name:   runGrubId,
								Hashes: []string{"run-grub-hash-1", "run-grub-hash-2"},
							},
						},
						Kernel:         "pc-kernel",
						KernelRevision: k.Revision,
						KernelCmdlines: []string{
							"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
						},
						KernelBootFile: k.BootFile,
					},
				}
				if shimId == "bootx64.efi" {
					expectedRunBootChainsForKernel = append(expectedExtraBootChains, expectedRunBootChainsForKernel...)
				} else {
					expectedRunBootChainsForKernel = append(expectedRunBootChainsForKernel, expectedExtraBootChains...)
				}
			}
			expectedRunBootChains = append(expectedRunBootChains, expectedRunBootChainsForKernel...)
		}

		// set mock key resealing
		resealKeysCalls := 0
		restore = boot.MockResealKeyForBootChains(func(unlocker boot.Unlocker, method device.SealingMethod, rootdirArg string, params *boot.ResealKeyForBootChainsParams) error {
			resealKeysCalls++

			c.Check(method, Equals, device.SealingMethodTPM)
			c.Check(params.Options.ExpectReseal, Equals, false)
			c.Check(rootdirArg, Equals, rootdir)

			c.Check(params.RunModeBootChains, DeepEquals, expectedRunBootChains)
			c.Check(params.RecoveryBootChainsForRunKey, DeepEquals, expectedRecoveryBootChains)
			c.Check(params.RecoveryBootChains, DeepEquals, expectedRecoveryBootChains)

			return tc.resealErr
		})
		defer restore()

		opts := boot.ResealKeyToModeenvOptions{ExpectReseal: false}
		err = boot.ResealKeyToModeenv(rootdir, modeenv, opts, nil)
		if !tc.sealedKeys {
			// did nothing
			c.Assert(err, IsNil)
			c.Assert(resealKeysCalls, Equals, 0)
			continue
		}

		if tc.err == "" {
			c.Assert(err, IsNil)
		} else {
			c.Assert(err, ErrorMatches, tc.err)
		}
		c.Assert(resealKeysCalls, Equals, 1)
		if tc.err != "" {
			continue
		}
	}
}

func (s *sealSuite) TestResealKeyToModeenvRecoveryKeysForGoodSystemsOnly(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	c.Assert(os.MkdirAll(dirs.SnapFDEDir, 0755), IsNil)
	err := os.WriteFile(filepath.Join(dirs.SnapFDEDir, "sealed-keys"), []byte(device.SealingMethodTPM), 0644)
	c.Assert(err, IsNil)

	err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-seed"))
	c.Assert(err, IsNil)

	err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-boot"))
	c.Assert(err, IsNil)

	model := boottest.MakeMockUC20Model()

	modeenv := &boot.Modeenv{
		// where 1234 is being tried
		CurrentRecoverySystems: []string{"20200825", "1234"},
		// 20200825 has known to be good
		GoodRecoverySystems: []string{"20200825"},
		CurrentTrustedRecoveryBootAssets: boot.BootAssetsMap{
			"grubx64.efi": []string{"grub-hash"},
			"bootx64.efi": []string{"shim-hash"},
		},

		CurrentTrustedBootAssets: boot.BootAssetsMap{
			"grubx64.efi": []string{"run-grub-hash"},
		},

		CurrentKernels: []string{"pc-kernel_500.snap"},

		CurrentKernelCommandLines: boot.BootCommandLines{
			"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
		},
		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
	}

	// set a mock recovery kernel
	readSystemEssentialCalls := 0
	restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
		readSystemEssentialCalls++
		kernelRev := 1
		if label == "1234" {
			kernelRev = 999
		}
		return model, []*seed.Snap{mockKernelSeedSnap(snap.R(kernelRev)), mockGadgetSeedSnap(c, nil)}, nil
	})
	defer restore()

	defer boot.MockModeenvLocked()()

	// set mock key resealing
	resealKeysCalls := 0
	restore = boot.MockResealKeyForBootChains(func(unlocker boot.Unlocker, method device.SealingMethod, rootdirArg string, params *boot.ResealKeyForBootChainsParams) error {
		resealKeysCalls++

		c.Check(method, Equals, device.SealingMethodTPM)
		c.Check(params.Options.ExpectReseal, Equals, false)
		c.Check(rootdirArg, Equals, rootdir)

		c.Assert(resealKeysCalls, Equals, 1)
		c.Check(params.RunModeBootChains, DeepEquals, []boot.BootChain{
			{
				BrandID:        "my-brand",
				Model:          "my-model-uc20",
				Grade:          "dangerous",
				ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
				AssetChain: []boot.BootAsset{
					{
						Role:   bootloader.RoleRecovery,
						Name:   "bootx64.efi",
						Hashes: []string{"shim-hash"},
					},
					{
						Role:   bootloader.RoleRecovery,
						Name:   "grubx64.efi",
						Hashes: []string{"grub-hash"},
					},
					{
						Role:   bootloader.RoleRunMode,
						Name:   "grubx64.efi",
						Hashes: []string{"run-grub-hash"},
					},
				},
				Kernel:         "pc-kernel",
				KernelRevision: "500",
				KernelCmdlines: []string{
					"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
				},
				KernelBootFile: bootloader.BootFile{
					Path: "kernel.efi",
					Snap: filepath.Join(rootdir, "var/lib/snapd/snaps/pc-kernel_500.snap"),
					Role: bootloader.RoleRunMode,
				},
			},
		})

		c.Check(params.RecoveryBootChainsForRunKey, DeepEquals, []boot.BootChain{
			{
				BrandID:        "my-brand",
				Model:          "my-model-uc20",
				Grade:          "dangerous",
				ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
				AssetChain: []boot.BootAsset{
					{
						Role:   bootloader.RoleRecovery,
						Name:   "bootx64.efi",
						Hashes: []string{"shim-hash"},
					},
					{
						Role:   bootloader.RoleRecovery,
						Name:   "grubx64.efi",
						Hashes: []string{"grub-hash"},
					},
				},
				Kernel:         "pc-kernel",
				KernelRevision: "1",
				KernelCmdlines: []string{
					"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
					"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				},
				KernelBootFile: bootloader.BootFile{
					Path: "kernel.efi",
					Snap: "/var/lib/snapd/seed/snaps/pc-kernel_1.snap",
					Role: bootloader.RoleRecovery,
				},
			},
			{
				BrandID:        "my-brand",
				Model:          "my-model-uc20",
				Grade:          "dangerous",
				ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
				AssetChain: []boot.BootAsset{
					{
						Role:   bootloader.RoleRecovery,
						Name:   "bootx64.efi",
						Hashes: []string{"shim-hash"},
					},
					{
						Role:   bootloader.RoleRecovery,
						Name:   "grubx64.efi",
						Hashes: []string{"grub-hash"},
					},
				},
				Kernel:         "pc-kernel",
				KernelRevision: "999",
				KernelCmdlines: []string{
					// but only the recover mode
					"snapd_recovery_mode=recover snapd_recovery_system=1234 console=ttyS0 console=tty1 panic=-1",
				},
				KernelBootFile: bootloader.BootFile{
					Path: "kernel.efi",
					Snap: "/var/lib/snapd/seed/snaps/pc-kernel_999.snap",
					Role: bootloader.RoleRecovery,
				},
			},
		})

		c.Check(params.RecoveryBootChains, DeepEquals, []boot.BootChain{
			{
				BrandID:        "my-brand",
				Model:          "my-model-uc20",
				Grade:          "dangerous",
				ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
				AssetChain: []boot.BootAsset{
					{
						Role:   bootloader.RoleRecovery,
						Name:   "bootx64.efi",
						Hashes: []string{"shim-hash"},
					},
					{
						Role:   bootloader.RoleRecovery,
						Name:   "grubx64.efi",
						Hashes: []string{"grub-hash"},
					},
				},
				Kernel:         "pc-kernel",
				KernelRevision: "1",
				KernelCmdlines: []string{
					"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
					"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				},
				KernelBootFile: bootloader.BootFile{
					Path: "kernel.efi",
					Snap: filepath.Join("/var/lib/snapd/seed/snaps/pc-kernel_1.snap"),
					Role: bootloader.RoleRecovery,
				},
			},
		})

		return nil
	})
	defer restore()

	// here we don't have unasserted kernels so just set
	// expectReseal to false as it doesn't matter;
	// the behavior with unasserted kernel is tested in
	// boot_test.go specific tests
	opts := boot.ResealKeyToModeenvOptions{ExpectReseal: false}
	err = boot.ResealKeyToModeenv(rootdir, modeenv, opts, nil)
	c.Assert(err, IsNil)
	c.Assert(resealKeysCalls, Equals, 1)
}

func (s *sealSuite) TestResealKeyToModeenvFallbackCmdline(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	model := boottest.MakeMockUC20Model()

	c.Assert(os.MkdirAll(dirs.SnapFDEDir, 0755), IsNil)
	err := os.WriteFile(filepath.Join(dirs.SnapFDEDir, "sealed-keys"), []byte(device.SealingMethodTPM), 0644)
	c.Assert(err, IsNil)

	modeenv := &boot.Modeenv{
		CurrentRecoverySystems: []string{"20200825"},
		CurrentTrustedRecoveryBootAssets: boot.BootAssetsMap{
			"asset": []string{"asset-hash-1"},
		},

		CurrentTrustedBootAssets: boot.BootAssetsMap{
			"asset": []string{"asset-hash-1"},
		},

		CurrentKernels: []string{"pc-kernel_500.snap"},

		// as if it is unset yet
		CurrentKernelCommandLines: nil,

		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
	}

	// match one of current kernels
	runKernelBf := bootloader.NewBootFile("/var/lib/snapd/snaps/pc-kernel_500.snap", "kernel.efi", bootloader.RoleRunMode)
	// match the seed kernel
	recoveryKernelBf := bootloader.NewBootFile("/var/lib/snapd/seed/snaps/pc-kernel_1.snap", "kernel.efi", bootloader.RoleRecovery)

	bootdir := c.MkDir()
	mtbl := bootloadertest.Mock("trusted", bootdir).WithTrustedAssets()
	mtbl.TrustedAssetsMap = map[string]string{"asset": "asset"}
	mtbl.StaticCommandLine = "static cmdline"
	mtbl.BootChainList = []bootloader.BootFile{
		bootloader.NewBootFile("", "asset", bootloader.RoleRunMode),
		runKernelBf,
	}
	mtbl.RecoveryBootChainList = []bootloader.BootFile{
		bootloader.NewBootFile("", "asset", bootloader.RoleRecovery),
		recoveryKernelBf,
	}
	bootloader.Force(mtbl)
	defer bootloader.Force(nil)

	// set a mock recovery kernel
	readSystemEssentialCalls := 0
	restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
		readSystemEssentialCalls++
		return model, []*seed.Snap{mockKernelSeedSnap(snap.R(1)), mockGadgetSeedSnap(c, nil)}, nil
	})
	defer restore()

	defer boot.MockModeenvLocked()()

	// set mock key resealing
	resealKeysCalls := 0
	restore = boot.MockResealKeyForBootChains(func(unlocker boot.Unlocker, method device.SealingMethod, rootdirArg string, params *boot.ResealKeyForBootChainsParams) error {
		c.Check(rootdirArg, Equals, rootdir)
		c.Check(method, Equals, device.SealingMethodTPM)
		c.Check(params.Options.ExpectReseal, Equals, false)

		resealKeysCalls++
		c.Logf("reseal: %+v", params)
		switch resealKeysCalls {
		case 1:
			c.Check(params.RunModeBootChains, DeepEquals, []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain: []boot.BootAsset{
						{
							Role:   "run-mode",
							Name:   "asset",
							Hashes: []string{"asset-hash-1"},
						},
					},
					Kernel:         "pc-kernel",
					KernelRevision: "500",
					KernelCmdlines: []string{
						"snapd_recovery_mode=run static cmdline",
					},
					KernelBootFile: bootloader.NewBootFile("/var/lib/snapd/snaps/pc-kernel_500.snap", "kernel.efi", bootloader.RoleRunMode),
				},
			})
			c.Check(params.RecoveryBootChainsForRunKey, DeepEquals, []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain: []boot.BootAsset{
						{
							Role:   "recovery",
							Name:   "asset",
							Hashes: []string{"asset-hash-1"},
						},
					},
					Kernel:         "pc-kernel",
					KernelRevision: "1",
					KernelCmdlines: []string{
						"snapd_recovery_mode=recover snapd_recovery_system=20200825 static cmdline",
					},
					KernelBootFile: bootloader.NewBootFile(filepath.Join("/var/lib/snapd/seed/snaps/pc-kernel_1.snap"), "kernel.efi", bootloader.RoleRecovery),
				},
			})
			c.Check(params.RecoveryBootChains, DeepEquals, []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain: []boot.BootAsset{
						{
							Role:   "recovery",
							Name:   "asset",
							Hashes: []string{"asset-hash-1"},
						},
					},
					Kernel:         "pc-kernel",
					KernelRevision: "1",
					KernelCmdlines: []string{
						"snapd_recovery_mode=recover snapd_recovery_system=20200825 static cmdline",
					},
					KernelBootFile: bootloader.NewBootFile(filepath.Join("/var/lib/snapd/seed/snaps/pc-kernel_1.snap"), "kernel.efi", bootloader.RoleRecovery),
				},
			})
		default:
			c.Fatalf("unexpected number of reseal calls, %v", params)
		}
		return nil
	})
	defer restore()

	opts := boot.ResealKeyToModeenvOptions{ExpectReseal: false}
	err = boot.ResealKeyToModeenv(rootdir, modeenv, opts, nil)
	c.Assert(err, IsNil)
	c.Assert(resealKeysCalls, Equals, 1)
}

func (s *sealSuite) TestRunModeBootChains(c *C) {
	for _, tc := range []struct {
		desc               string
		cmdlines           []string
		recoveryAssetsMap  boot.BootAssetsMap
		runAssetsMap       boot.BootAssetsMap
		currentKernels     []string
		expectedCmdlines   [][]string
		expectedAssets     [][]boot.BootAsset
		expectedKernelRevs []int
		expectedErr        string
	}{
		{
			desc:     "Old chain",
			cmdlines: []string{"testline"},
			recoveryAssetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			runAssetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-2", "grub-hash-3"},
			},
			currentKernels:     []string{"pc-kernel_500.snap"},
			expectedKernelRevs: []int{500, 500},
			expectedCmdlines: [][]string{
				{"testline"},
				{"testline"},
			},
			expectedAssets: [][]boot.BootAsset{
				{
					{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
					{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1"}},
					{Role: bootloader.RoleRunMode, Name: "grubx64.efi", Hashes: []string{"grub-hash-2", "grub-hash-3"}},
				},
			},
		},
		{
			desc:     "New chain",
			cmdlines: []string{"testline"},
			recoveryAssetsMap: boot.BootAssetsMap{
				"ubuntu:grubx64.efi": []string{"grub-hash-1"},
				"ubuntu:shimx64.efi": []string{"shim-hash-1"},
			},
			runAssetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-2", "grub-hash-3"},
			},
			currentKernels:     []string{"pc-kernel_500.snap"},
			expectedKernelRevs: []int{500, 500},
			expectedCmdlines: [][]string{
				{"testline"},
				{"testline"},
			},
			expectedAssets: [][]boot.BootAsset{
				{
					{Role: bootloader.RoleRecovery, Name: "ubuntu:shimx64.efi", Hashes: []string{"shim-hash-1"}},
					{Role: bootloader.RoleRecovery, Name: "ubuntu:grubx64.efi", Hashes: []string{"grub-hash-1"}},
					{Role: bootloader.RoleRunMode, Name: "grubx64.efi", Hashes: []string{"grub-hash-2", "grub-hash-3"}},
				},
			},
		},
		{
			desc:     "Both old and new chains",
			cmdlines: []string{"testline"},
			recoveryAssetsMap: boot.BootAssetsMap{
				"grubx64.efi":        []string{"grub-hash-1"},
				"bootx64.efi":        []string{"shim-hash-1"},
				"ubuntu:grubx64.efi": []string{"grub-hash-3"},
				"ubuntu:shimx64.efi": []string{"shim-hash-3"},
			},
			runAssetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-2", "grub-hash-3"},
			},
			currentKernels:     []string{"pc-kernel_500.snap"},
			expectedKernelRevs: []int{500, 500},
			expectedCmdlines: [][]string{
				{"testline"},
				{"testline"},
			},
			expectedAssets: [][]boot.BootAsset{
				{
					{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
					{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1"}},
					{Role: bootloader.RoleRunMode, Name: "grubx64.efi", Hashes: []string{"grub-hash-2", "grub-hash-3"}},
				},
				{
					{Role: bootloader.RoleRecovery, Name: "ubuntu:shimx64.efi", Hashes: []string{"shim-hash-3"}},
					{Role: bootloader.RoleRecovery, Name: "ubuntu:grubx64.efi", Hashes: []string{"grub-hash-3"}},
					{Role: bootloader.RoleRunMode, Name: "grubx64.efi", Hashes: []string{"grub-hash-2", "grub-hash-3"}},
				},
			},
		},
	} {
		c.Logf("tc: %q", tc.desc)
		rootdir := c.MkDir()
		dirs.SetRootDir(rootdir)
		defer dirs.SetRootDir("")

		model := boottest.MakeMockUC20Model()

		modeenv := &boot.Modeenv{
			CurrentTrustedRecoveryBootAssets: tc.recoveryAssetsMap,
			CurrentTrustedBootAssets:         tc.runAssetsMap,
			CurrentKernels:                   tc.currentKernels,

			BrandID:        model.BrandID(),
			Model:          model.Model(),
			ModelSignKeyID: model.SignKeyID(),
			Grade:          string(model.Grade()),
		}

		grubDir := filepath.Join(rootdir, "run/mnt/ubuntu-seed")
		err := createMockGrubCfg(grubDir)
		c.Assert(err, IsNil)

		runGrubDir := filepath.Join(rootdir, "run/mnt/ubuntu-boot")
		err = createMockGrubCfg(runGrubDir)
		c.Assert(err, IsNil)

		rbl, err := bootloader.Find(grubDir, &bootloader.Options{
			Role:        bootloader.RoleRecovery,
			NoSlashBoot: true,
		})
		c.Assert(err, IsNil)
		bl, err := bootloader.Find(runGrubDir, &bootloader.Options{
			Role:        bootloader.RoleRunMode,
			NoSlashBoot: true,
		})
		c.Assert(err, IsNil)

		tbl, ok := rbl.(bootloader.TrustedAssetsBootloader)
		c.Assert(ok, Equals, true)

		bootChains, err := boot.RunModeBootChains(tbl, bl, modeenv, tc.cmdlines, "/snaps")
		if tc.expectedErr == "" {
			c.Assert(err, IsNil)

			foundChains := make(map[int]bool)
			for i, chain := range bootChains {
				foundChain := false
				c.Logf("For chain: %v", chain.AssetChain)
				for j, expectedAssets := range tc.expectedAssets {
					c.Logf("Comparing with: %v", expectedAssets)
					if reflect.DeepEqual(chain.AssetChain, expectedAssets) {
						foundChains[j] = true
						foundChain = true
						continue
					}
				}
				c.Assert(foundChain, Equals, true)
				c.Assert(chain.Kernel, Equals, "pc-kernel")
				expectedKernelRev := tc.expectedKernelRevs[i]
				c.Assert(chain.KernelRevision, Equals, fmt.Sprintf("%d", expectedKernelRev))
				c.Assert(chain.KernelBootFile, DeepEquals, bootloader.BootFile{
					Snap: fmt.Sprintf("/snaps/pc-kernel_%d.snap", expectedKernelRev),
					Path: "kernel.efi",
					Role: bootloader.RoleRunMode,
				})
				c.Assert(chain.KernelCmdlines, DeepEquals, tc.expectedCmdlines[i])
			}
			for j := range tc.expectedAssets {
				c.Assert(foundChains[j], Equals, true)
			}
		} else {
			c.Assert(err, ErrorMatches, tc.expectedErr)
		}
	}
}

func (s *sealSuite) TestRecoveryBootChainsForSystems(c *C) {
	for _, tc := range []struct {
		desc                    string
		assetsMap               boot.BootAssetsMap
		recoverySystems         []string
		modesForSystems         map[string][]string
		undefinedKernel         bool
		gadgetFilesForSystem    map[string][][]string
		expectedAssets          [][]boot.BootAsset
		expectedKernelRevs      []int
		expectedBootChainsCount int
		// in the order of boot chains
		expectedCmdlines [][]string
		err              string
	}{
		{
			desc:            "transition sequences",
			recoverySystems: []string{"20200825"},
			modesForSystems: map[string][]string{"20200825": {boot.ModeRecover, boot.ModeFactoryReset}},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1", "grub-hash-2"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1", "grub-hash-2"}},
			}},
			expectedKernelRevs: []int{1},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}},
		},
		{
			desc:            "two systems",
			recoverySystems: []string{"20200825", "20200831"},
			modesForSystems: map[string][]string{
				"20200825": {boot.ModeRecover, boot.ModeFactoryReset},
				"20200831": {boot.ModeRecover, boot.ModeFactoryReset},
			},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1", "grub-hash-2"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1", "grub-hash-2"}},
			}},
			expectedKernelRevs: []int{1, 3},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}, {
				"snapd_recovery_mode=recover snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1",
			}},
		},
		{
			desc:            "non transition sequence",
			recoverySystems: []string{"20200825"},
			modesForSystems: map[string][]string{"20200825": {boot.ModeRecover, boot.ModeFactoryReset}},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1"}},
			}},
			expectedKernelRevs: []int{1},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}},
		},
		{
			desc:            "two systems with command lines",
			recoverySystems: []string{"20200825", "20200831"},
			modesForSystems: map[string][]string{
				"20200825": {boot.ModeRecover, boot.ModeFactoryReset},
				"20200831": {boot.ModeRecover, boot.ModeFactoryReset},
			},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1", "grub-hash-2"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1", "grub-hash-2"}},
			}},
			gadgetFilesForSystem: map[string][][]string{
				"20200825": {
					{"cmdline.extra", "extra for 20200825"},
				},
				"20200831": {
					// TODO: make it a cmdline.full
					{"cmdline.extra", "some-extra-for-20200831"},
				},
			},
			expectedKernelRevs: []int{1, 3},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1 extra for 20200825",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1 extra for 20200825",
			}, {
				"snapd_recovery_mode=recover snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1 some-extra-for-20200831",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1 some-extra-for-20200831",
			}},
		},
		{
			desc:            "three systems, one with different model",
			recoverySystems: []string{"20200825", "20200831", "off-model"},
			modesForSystems: map[string][]string{
				"20200825":  {boot.ModeRecover, boot.ModeFactoryReset},
				"20200831":  {boot.ModeRecover, boot.ModeFactoryReset},
				"off-model": {boot.ModeRecover, boot.ModeFactoryReset},
			},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1", "grub-hash-2"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1", "grub-hash-2"}},
			}},
			expectedKernelRevs: []int{1, 3},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}, {
				"snapd_recovery_mode=recover snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1",
			}},
			expectedBootChainsCount: 2,
		},
		{
			desc:            "two systems, one with different modes",
			recoverySystems: []string{"20200825", "20200831"},
			modesForSystems: map[string][]string{
				"20200825": {boot.ModeRecover, boot.ModeFactoryReset},
				"20200831": {boot.ModeRecover},
			},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1", "grub-hash-2"},
				"bootx64.efi": []string{"shim-hash-1"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1", "grub-hash-2"}},
			}},
			expectedKernelRevs: []int{1, 3},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}, {
				"snapd_recovery_mode=recover snapd_recovery_system=20200831 console=ttyS0 console=tty1 panic=-1",
			}},
			expectedBootChainsCount: 2,
		},
		{
			desc:            "invalid recovery system label",
			recoverySystems: []string{"0"},
			modesForSystems: map[string][]string{"0": {boot.ModeRecover}},
			err:             `cannot read system "0" seed: invalid system seed`,
		},
		{
			desc:            "missing modes for a system",
			recoverySystems: []string{"20200825"},
			modesForSystems: map[string][]string{"other": {boot.ModeRecover}},
			err:             `internal error: no modes for system "20200825"`,
		},
		{
			desc:            "no matching boot chains",
			recoverySystems: []string{"20200825"},
			modesForSystems: map[string][]string{"20200825": {boot.ModeRecover, boot.ModeFactoryReset}},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi": []string{"grub-hash-1", "grub-hash-2"},
				"shimx64.efi": []string{"shim-hash-1"}, // it should be bootx64.efi
			},
			err: `could not find any valid chain for this model`,
		},
		{
			desc:            "udpate to new layout",
			recoverySystems: []string{"20200825"},
			modesForSystems: map[string][]string{"20200825": {boot.ModeRecover, boot.ModeFactoryReset}},
			assetsMap: boot.BootAssetsMap{
				"grubx64.efi":        []string{"grub-hash-1"},
				"bootx64.efi":        []string{"shim-hash-1"},
				"ubuntu:grubx64.efi": []string{"grub-hash-2"},
				"ubuntu:shimx64.efi": []string{"shim-hash-2"},
			},
			expectedAssets: [][]boot.BootAsset{{
				{Role: bootloader.RoleRecovery, Name: "bootx64.efi", Hashes: []string{"shim-hash-1"}},
				{Role: bootloader.RoleRecovery, Name: "grubx64.efi", Hashes: []string{"grub-hash-1"}},
			}, {
				{Role: bootloader.RoleRecovery, Name: "ubuntu:shimx64.efi", Hashes: []string{"shim-hash-2"}},
				{Role: bootloader.RoleRecovery, Name: "ubuntu:grubx64.efi", Hashes: []string{"grub-hash-2"}},
			}},
			expectedKernelRevs: []int{1, 1},
			expectedCmdlines: [][]string{{
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}, {
				"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
				"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
			}},
			expectedBootChainsCount: 2,
		},
	} {
		c.Logf("tc: %q", tc.desc)
		rootdir := c.MkDir()
		dirs.SetRootDir(rootdir)
		defer dirs.SetRootDir("")

		model := boottest.MakeMockUC20Model()

		// set recovery kernel
		restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
			systemModel := model
			kernelRev := 1
			switch label {
			case "20200825":
				// nothing special
			case "20200831":
				kernelRev = 3
			case "off-model":
				systemModel = boottest.MakeMockUC20Model(map[string]any{
					"model": "model-mismatch-uc20",
				})
			default:
				return nil, nil, fmt.Errorf("invalid system seed")
			}
			return systemModel, []*seed.Snap{mockKernelSeedSnap(snap.R(kernelRev)), mockGadgetSeedSnap(c, tc.gadgetFilesForSystem[label])}, nil
		})
		defer restore()

		grubDir := filepath.Join(rootdir, "run/mnt/ubuntu-seed")
		err := createMockGrubCfg(grubDir)
		c.Assert(err, IsNil)

		bl, err := bootloader.Find(grubDir, &bootloader.Options{Role: bootloader.RoleRecovery})
		c.Assert(err, IsNil)
		tbl, ok := bl.(bootloader.TrustedAssetsBootloader)
		c.Assert(ok, Equals, true)

		modeenv := &boot.Modeenv{
			CurrentTrustedRecoveryBootAssets: tc.assetsMap,

			BrandID:        model.BrandID(),
			Model:          model.Model(),
			ModelSignKeyID: model.SignKeyID(),
			Grade:          string(model.Grade()),
		}

		includeTryModel := false
		bc, err := boot.RecoveryBootChainsForSystems(tc.recoverySystems, tc.modesForSystems, tbl, modeenv, includeTryModel, dirs.SnapSeedDir)
		if tc.err == "" {
			c.Assert(err, IsNil)
			if tc.expectedBootChainsCount == 0 {
				// usually there is a boot chain for each recovery system
				c.Assert(bc, HasLen, len(tc.recoverySystems))
			} else {
				c.Assert(bc, HasLen, tc.expectedBootChainsCount)
			}
			c.Assert(tc.expectedCmdlines, HasLen, len(bc), Commentf("broken test, expected command lines must be of the same length as recovery systems and recovery boot chains"))
			foundChains := make(map[int]bool)
			for i, chain := range bc {
				foundChain := false
				for j, expectedAssets := range tc.expectedAssets {
					if reflect.DeepEqual(chain.AssetChain, expectedAssets) {
						foundChains[j] = true
						foundChain = true
						continue
					}
				}
				c.Assert(foundChain, Equals, true)
				c.Assert(chain.Kernel, Equals, "pc-kernel")
				expectedKernelRev := tc.expectedKernelRevs[i]
				c.Assert(chain.KernelRevision, Equals, fmt.Sprintf("%d", expectedKernelRev))
				c.Assert(chain.KernelBootFile, DeepEquals, bootloader.BootFile{
					Snap: fmt.Sprintf("/var/lib/snapd/seed/snaps/pc-kernel_%d.snap", expectedKernelRev),
					Path: "kernel.efi",
					Role: bootloader.RoleRecovery,
				})
				c.Assert(chain.KernelCmdlines, DeepEquals, tc.expectedCmdlines[i])
			}
			for j := range tc.expectedAssets {
				c.Assert(foundChains[j], Equals, true)
			}
		} else {
			c.Assert(err, ErrorMatches, tc.err)
		}

	}

}

func createMockGrubCfg(baseDir string) error {
	cfg := filepath.Join(baseDir, "EFI/ubuntu/grub.cfg")
	if err := os.MkdirAll(filepath.Dir(cfg), 0755); err != nil {
		return err
	}
	return os.WriteFile(cfg, []byte("# Snapd-Boot-Config-Edition: 1\n"), 0644)
}

func (s *sealSuite) TestSealKeyModelParams(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	model := boottest.MakeMockUC20Model()

	roleToBlName := map[bootloader.Role]string{
		bootloader.RoleRecovery: "grub",
		bootloader.RoleRunMode:  "grub",
	}
	// mock asset cache
	boottest.MockAssetsCache(c, rootdir, "grub", []string{
		"shim-shim-hash",
		"loader-loader-hash1",
		"loader-loader-hash2",
	})

	oldmodel := boottest.MakeMockUC20Model(map[string]any{
		"model":     "old-model-uc20",
		"timestamp": "2019-10-01T08:00:00+00:00",
	})

	// old recovery
	oldrc := boot.BootChain{
		BrandID:        oldmodel.BrandID(),
		Model:          oldmodel.Model(),
		Grade:          oldmodel.Grade(),
		ModelSignKeyID: oldmodel.SignKeyID(),
		AssetChain: []boot.BootAsset{
			{Name: "shim", Role: bootloader.RoleRecovery, Hashes: []string{"shim-hash"}},
			{Name: "loader", Role: bootloader.RoleRecovery, Hashes: []string{"loader-hash1"}},
		},
		KernelCmdlines: []string{"panic=1", "oldrc"},
	}
	oldkbf := bootloader.BootFile{Snap: "pc-kernel_1.snap"}
	oldrc.SetKernelBootFile(oldkbf)

	// recovery
	rc1 := boot.BootChain{
		BrandID:        model.BrandID(),
		Model:          model.Model(),
		Grade:          model.Grade(),
		ModelSignKeyID: model.SignKeyID(),
		AssetChain: []boot.BootAsset{
			{Name: "shim", Role: bootloader.RoleRecovery, Hashes: []string{"shim-hash"}},
			{Name: "loader", Role: bootloader.RoleRecovery, Hashes: []string{"loader-hash1"}},
		},
		KernelCmdlines: []string{"panic=1", "rc1"},
	}
	rc1kbf := bootloader.BootFile{Snap: "pc-kernel_10.snap"}
	rc1.SetKernelBootFile(rc1kbf)

	// run system
	runc1 := boot.BootChain{
		BrandID:        model.BrandID(),
		Model:          model.Model(),
		Grade:          model.Grade(),
		ModelSignKeyID: model.SignKeyID(),
		AssetChain: []boot.BootAsset{
			{Name: "shim", Role: bootloader.RoleRecovery, Hashes: []string{"shim-hash"}},
			{Name: "loader", Role: bootloader.RoleRecovery, Hashes: []string{"loader-hash1"}},
			{Name: "loader", Role: bootloader.RoleRunMode, Hashes: []string{"loader-hash2"}},
		},
		KernelCmdlines: []string{"panic=1", "runc1"},
	}
	runc1kbf := bootloader.BootFile{Snap: "pc-kernel_50.snap"}
	runc1.SetKernelBootFile(runc1kbf)

	pbc := boot.ToPredictableBootChains([]boot.BootChain{rc1, runc1, oldrc})

	shim := bootloader.NewBootFile("", filepath.Join(rootdir, "var/lib/snapd/boot-assets/grub/shim-shim-hash"), bootloader.RoleRecovery)
	loader1 := bootloader.NewBootFile("", filepath.Join(rootdir, "var/lib/snapd/boot-assets/grub/loader-loader-hash1"), bootloader.RoleRecovery)
	loader2 := bootloader.NewBootFile("", filepath.Join(rootdir, "var/lib/snapd/boot-assets/grub/loader-loader-hash2"), bootloader.RoleRunMode)

	params, err := boot.SealKeyModelParams(pbc, roleToBlName)
	c.Assert(err, IsNil)
	c.Check(params, HasLen, 2)
	c.Check(params[0].Model.Model(), Equals, model.Model())
	// NB: merging of lists makes panic=1 appear once
	c.Check(params[0].KernelCmdlines, DeepEquals, []string{"panic=1", "rc1", "runc1"})

	c.Check(params[0].EFILoadChains, DeepEquals, []*secboot.LoadChain{
		secboot.NewLoadChain(shim,
			secboot.NewLoadChain(loader1,
				secboot.NewLoadChain(rc1kbf))),
		secboot.NewLoadChain(shim,
			secboot.NewLoadChain(loader1,
				secboot.NewLoadChain(loader2,
					secboot.NewLoadChain(runc1kbf)))),
	})

	c.Check(params[1].Model.Model(), Equals, oldmodel.Model())
	c.Check(params[1].KernelCmdlines, DeepEquals, []string{"oldrc", "panic=1"})
	c.Check(params[1].EFILoadChains, DeepEquals, []*secboot.LoadChain{
		secboot.NewLoadChain(shim,
			secboot.NewLoadChain(loader1,
				secboot.NewLoadChain(oldkbf))),
	})
}

func (s *sealSuite) TestIsResealNeeded(c *C) {
	if os.Geteuid() == 0 {
		c.Skip("the test cannot be run by the root user")
	}

	chains := []boot.BootChain{
		{
			BrandID:        "mybrand",
			Model:          "foo",
			Grade:          "signed",
			ModelSignKeyID: "my-key-id",
			AssetChain: []boot.BootAsset{
				// hashes will be sorted
				{Role: bootloader.RoleRecovery, Name: "shim", Hashes: []string{"x", "y"}},
				{Role: bootloader.RoleRecovery, Name: "loader", Hashes: []string{"c", "d"}},
				{Role: bootloader.RoleRunMode, Name: "loader", Hashes: []string{"z", "x"}},
			},
			Kernel:         "pc-kernel-other",
			KernelRevision: "2345",
			KernelCmdlines: []string{`snapd_recovery_mode=run foo`},
		}, {
			BrandID:        "mybrand",
			Model:          "foo",
			Grade:          "dangerous",
			ModelSignKeyID: "my-key-id",
			AssetChain: []boot.BootAsset{
				// hashes will be sorted
				{Role: bootloader.RoleRecovery, Name: "shim", Hashes: []string{"y", "x"}},
				{Role: bootloader.RoleRecovery, Name: "loader", Hashes: []string{"c", "d"}},
			},
			Kernel:         "pc-kernel-recovery",
			KernelRevision: "1234",
			KernelCmdlines: []string{`snapd_recovery_mode=recover foo`},
		},
	}

	pbc := boot.ToPredictableBootChains(chains)

	rootdir := c.MkDir()
	err := boot.WriteBootChains(pbc, filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains"), 2)
	c.Assert(err, IsNil)

	needed, _, err := boot.IsResealNeeded(pbc, filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains"), false)
	c.Assert(err, IsNil)
	c.Check(needed, Equals, false)

	otherchain := []boot.BootChain{pbc[0]}
	needed, cnt, err := boot.IsResealNeeded(otherchain, filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains"), false)
	c.Assert(err, IsNil)
	// chains are different
	c.Check(needed, Equals, true)
	c.Check(cnt, Equals, 3)

	// boot-chains does not exist, we cannot compare so advise to reseal
	otherRootdir := c.MkDir()
	needed, cnt, err = boot.IsResealNeeded(otherchain, filepath.Join(dirs.SnapFDEDirUnder(otherRootdir), "boot-chains"), false)
	c.Assert(err, IsNil)
	c.Check(needed, Equals, true)
	c.Check(cnt, Equals, 1)

	// exists but cannot be read
	c.Assert(os.Chmod(filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains"), 0000), IsNil)
	defer os.Chmod(filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains"), 0755)
	needed, _, err = boot.IsResealNeeded(otherchain, filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains"), false)
	c.Assert(err, ErrorMatches, "cannot open existing boot chains data file: open .*/boot-chains: permission denied")
	c.Check(needed, Equals, false)

	// unrevisioned kernel chain
	unrevchain := []boot.BootChain{pbc[0], pbc[1]}
	unrevchain[1].KernelRevision = ""
	// write on disk
	bootChainsFile := filepath.Join(dirs.SnapFDEDirUnder(rootdir), "boot-chains")
	err = boot.WriteBootChains(unrevchain, bootChainsFile, 2)
	c.Assert(err, IsNil)

	needed, cnt, err = boot.IsResealNeeded(pbc, bootChainsFile, false)
	c.Assert(err, IsNil)
	c.Check(needed, Equals, true)
	c.Check(cnt, Equals, 3)

	// cases falling back to expectReseal
	needed, _, err = boot.IsResealNeeded(unrevchain, bootChainsFile, false)
	c.Assert(err, IsNil)
	c.Check(needed, Equals, false)

	needed, cnt, err = boot.IsResealNeeded(unrevchain, bootChainsFile, true)
	c.Assert(err, IsNil)
	c.Check(needed, Equals, true)
	c.Check(cnt, Equals, 3)
}

func (s *sealSuite) TestSealToModeenvWithFdeHookHappy(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")
	model := boottest.MakeMockUC20Model()

	restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
		return model, []*seed.Snap{mockKernelSeedSnap(snap.R(1)), mockGadgetSeedSnap(c, nil)}, nil
	})
	defer restore()

	modeenv := &boot.Modeenv{
		RecoverySystem: "20200825",
		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
	}

	myKey := secboot.CreateMockBootstrappedContainer()
	myKey2 := secboot.CreateMockBootstrappedContainer()

	sealKeyForBootChainsCalled := 0
	restore = boot.MockSealKeyForBootChains(func(method device.SealingMethod, key, saveKey secboot.BootstrappedContainer, primaryKey []byte, volumesAuth *device.VolumesAuthOptions, params *boot.SealKeyForBootChainsParams) error {
		sealKeyForBootChainsCalled++
		c.Check(method, Equals, device.SealingMethodFDESetupHook)
		c.Check(key, DeepEquals, myKey)
		c.Check(saveKey, DeepEquals, myKey2)

		c.Assert(params.RunModeBootChains, HasLen, 1)
		runBootChain := params.RunModeBootChains[0]
		c.Check(runBootChain.Model, Equals, model.Model())

		c.Check(runBootChain.AssetChain, HasLen, 0)

		c.Check(params.RecoveryBootChainsForRunKey, HasLen, 0)

		c.Assert(params.RecoveryBootChains, HasLen, 1)
		recoveryBootChain := params.RecoveryBootChains[0]
		c.Check(recoveryBootChain.Model, Equals, model.Model())
		c.Check(recoveryBootChain.AssetChain, HasLen, 0)

		c.Check(params.FactoryReset, Equals, false)
		c.Check(params.InstallHostWritableDir, Equals, filepath.Join(boot.InitramfsRunMntDir, "ubuntu-data", "system-data"))
		c.Check(params.UseTokens, Equals, true)

		return nil
	})
	defer restore()

	defer boot.MockSealModeenvLocked()()

	err := boot.SealKeyToModeenv(myKey, myKey2, nil, nil, model, modeenv, boot.MockSealKeyToModeenvFlags{HookKeyProtectorFactory: &fakeProtectorFactory{}, UseTokens: true})
	c.Assert(err, IsNil)
	c.Check(sealKeyForBootChainsCalled, Equals, 1)
}

func (s *sealSuite) TestSealToModeenvWithFdeHookSad(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	model := boottest.MakeMockUC20Model()

	sealKeyForBootChainsCalled := 0
	restore := boot.MockSealKeyForBootChains(func(method device.SealingMethod, key, saveKey secboot.BootstrappedContainer, primaryKey []byte, volumesAuth *device.VolumesAuthOptions, params *boot.SealKeyForBootChainsParams) error {
		sealKeyForBootChainsCalled++

		return fmt.Errorf("seal key failed")
	})
	defer restore()

	restore = boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
		return model, []*seed.Snap{mockKernelSeedSnap(snap.R(1)), mockGadgetSeedSnap(c, nil)}, nil
	})
	defer restore()

	modeenv := &boot.Modeenv{
		RecoverySystem: "20200825",
	}
	key := secboot.CreateMockBootstrappedContainer()
	saveKey := secboot.CreateMockBootstrappedContainer()

	defer boot.MockSealModeenvLocked()()

	err := boot.SealKeyToModeenv(key, saveKey, nil, nil, model, modeenv, boot.MockSealKeyToModeenvFlags{HookKeyProtectorFactory: &fakeProtectorFactory{}})
	c.Assert(err, ErrorMatches, `seal key failed`)
	c.Check(sealKeyForBootChainsCalled, Equals, 1)
}

func (s *sealSuite) TestResealKeyToModeenvWithFdeHookCalled(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	mockResealKeyForBootChainsCalls := 0
	restore := boot.MockResealKeyForBootChains(func(unlocker boot.Unlocker, method device.SealingMethod, rootdirArg string, params *boot.ResealKeyForBootChainsParams) error {
		c.Check(rootdirArg, Equals, rootdir)
		c.Check(method, Equals, device.SealingMethodFDESetupHook)
		c.Check(params.Options.ExpectReseal, Equals, false)

		mockResealKeyForBootChainsCalls++
		return nil
	})
	defer restore()

	// TODO: this simulates that the hook is not available yet
	//       because of e.g. seeding. Longer term there will be
	//       more, see TODO in resealKeyToModeenvUsingFDESetupHookImpl
	restore = boot.MockHookKeyProtectorFactory(func(kernel *snap.Info) (secboot.KeyProtectorFactory, error) {
		return nil, errors.New("hook not available yet because e.g. seeding")
	})
	defer restore()

	marker := filepath.Join(dirs.SnapFDEDirUnder(rootdir), "sealed-keys")
	err := os.MkdirAll(filepath.Dir(marker), 0755)
	c.Assert(err, IsNil)
	err = os.WriteFile(marker, []byte("fde-setup-hook"), 0644)
	c.Assert(err, IsNil)

	defer boot.MockModeenvLocked()()

	model := boottest.MakeMockUC20Model()
	modeenv := &boot.Modeenv{
		RecoverySystem: "20200825",
		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
	}
	opts := boot.ResealKeyToModeenvOptions{ExpectReseal: false}
	err = boot.ResealKeyToModeenv(rootdir, modeenv, opts, nil)
	c.Assert(err, IsNil)
	c.Check(mockResealKeyForBootChainsCalls, Equals, 1)
}

func (s *sealSuite) TestResealKeyToModeenvWithFdeHookVerySad(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	mockResealKeyForBootChainsCalls := 0
	restore := boot.MockResealKeyForBootChains(func(unlocker boot.Unlocker, method device.SealingMethod, rootdirArg string, params *boot.ResealKeyForBootChainsParams) error {
		c.Check(rootdirArg, Equals, rootdir)
		c.Check(method, Equals, device.SealingMethodFDESetupHook)
		c.Check(params.Options.ExpectReseal, Equals, false)
		mockResealKeyForBootChainsCalls++
		return fmt.Errorf("fde setup hook failed")
	})
	defer restore()

	marker := filepath.Join(dirs.SnapFDEDirUnder(rootdir), "sealed-keys")
	err := os.MkdirAll(filepath.Dir(marker), 0755)
	c.Assert(err, IsNil)
	err = os.WriteFile(marker, []byte("fde-setup-hook"), 0644)
	c.Assert(err, IsNil)

	defer boot.MockModeenvLocked()()

	model := boottest.MakeMockUC20Model()
	modeenv := &boot.Modeenv{
		RecoverySystem: "20200825",
		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
	}
	opts := boot.ResealKeyToModeenvOptions{ExpectReseal: false}
	err = boot.ResealKeyToModeenv(rootdir, modeenv, opts, nil)
	c.Assert(err, ErrorMatches, "fde setup hook failed")
	c.Check(mockResealKeyForBootChainsCalls, Equals, 1)
}

func (s *sealSuite) testResealKeyToModeenvWithTryModel(c *C, shimId, grubId string) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	c.Assert(os.MkdirAll(dirs.SnapFDEDir, 0755), IsNil)
	err := os.WriteFile(filepath.Join(dirs.SnapFDEDir, "sealed-keys"), []byte(device.SealingMethodTPM), 0644)
	c.Assert(err, IsNil)

	err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-seed"))
	c.Assert(err, IsNil)

	err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-boot"))
	c.Assert(err, IsNil)

	model := boottest.MakeMockUC20Model()
	// a try model which would normally only appear during remodel
	tryModel := boottest.MakeMockUC20Model(map[string]any{
		"model": "try-my-model-uc20",
		"grade": "secured",
	})

	modeenv := &boot.Modeenv{
		// recovery system set up like during a remodel, right before a
		// set-device is called, the recovery system of the new model
		// has been tested
		CurrentRecoverySystems: []string{"20200825", "1234", "off-model"},
		GoodRecoverySystems:    []string{"20200825", "1234"},

		CurrentTrustedRecoveryBootAssets: boot.BootAssetsMap{
			grubId: []string{"grub-hash"},
			shimId: []string{"shim-hash"},
		},

		CurrentTrustedBootAssets: boot.BootAssetsMap{
			"grubx64.efi": []string{"run-grub-hash"},
		},

		CurrentKernels: []string{"pc-kernel_500.snap"},

		CurrentKernelCommandLines: boot.BootCommandLines{
			"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
		},
		// the current model
		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
		// the try model
		TryModel:          tryModel.Model(),
		TryBrandID:        tryModel.BrandID(),
		TryGrade:          string(tryModel.Grade()),
		TryModelSignKeyID: tryModel.SignKeyID(),
	}

	// set a mock recovery kernel
	readSystemEssentialCalls := 0
	restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
		readSystemEssentialCalls++
		kernelRev := 1
		systemModel := model
		if label == "1234" {
			// recovery system for new model
			kernelRev = 999
			systemModel = tryModel
		}
		if label == "off-model" {
			// a model that matches neither current not try models
			systemModel = boottest.MakeMockUC20Model(map[string]any{
				"model": "different-model-uc20",
				"grade": "secured",
			})
		}
		return systemModel, []*seed.Snap{mockKernelSeedSnap(snap.R(kernelRev)), mockGadgetSeedSnap(c, nil)}, nil
	})
	defer restore()

	defer boot.MockModeenvLocked()()

	// set mock key resealing
	resealKeysCalls := 0
	restore = boot.MockResealKeyForBootChains(func(unlocker boot.Unlocker, method device.SealingMethod, rootdirArg string, params *boot.ResealKeyForBootChainsParams) error {
		c.Check(rootdirArg, Equals, rootdir)
		c.Check(method, Equals, device.SealingMethodTPM)
		c.Check(params.Options.ExpectReseal, Equals, false)
		resealKeysCalls++

		kernelOldRecovery := bootloader.NewBootFile("/var/lib/snapd/seed/snaps/pc-kernel_1.snap", "kernel.efi", bootloader.RoleRecovery)
		kernelNewRecovery := bootloader.NewBootFile("/var/lib/snapd/seed/snaps/pc-kernel_999.snap", "kernel.efi", bootloader.RoleRecovery)
		runKernel := bootloader.NewBootFile(filepath.Join(rootdir, "var/lib/snapd/snaps/pc-kernel_500.snap"), "kernel.efi", bootloader.RoleRunMode)

		recoveryAssetChain := []boot.BootAsset{{
			Role:   "recovery",
			Name:   shimId,
			Hashes: []string{"shim-hash"},
		}, {
			Role:   "recovery",
			Name:   grubId,
			Hashes: []string{"grub-hash"},
		}}
		runAssetChain := []boot.BootAsset{{
			Role:   "recovery",
			Name:   shimId,
			Hashes: []string{"shim-hash"},
		}, {
			Role:   "recovery",
			Name:   grubId,
			Hashes: []string{"grub-hash"},
		}, {
			Role:   "run-mode",
			Name:   "grubx64.efi",
			Hashes: []string{"run-grub-hash"},
		}}

		switch resealKeysCalls {
		case 1:
			c.Check(params.RunModeBootChains, DeepEquals, []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain:     runAssetChain,
					Kernel:         "pc-kernel",
					KernelRevision: "500",
					KernelCmdlines: []string{
						"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: runKernel,
				},
				{
					BrandID:        "my-brand",
					Model:          "try-my-model-uc20",
					Grade:          "secured",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain:     runAssetChain,
					Kernel:         "pc-kernel",
					KernelRevision: "500",
					KernelCmdlines: []string{
						"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: runKernel,
				},
			})
			c.Check(params.RecoveryBootChainsForRunKey, DeepEquals, []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain:     recoveryAssetChain,
					Kernel:         "pc-kernel",
					KernelRevision: "1",
					KernelCmdlines: []string{
						"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
						"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: kernelOldRecovery,
				},
				{
					BrandID:        "my-brand",
					Model:          "try-my-model-uc20",
					Grade:          "secured",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain:     recoveryAssetChain,
					Kernel:         "pc-kernel",
					KernelRevision: "999",
					KernelCmdlines: []string{
						"snapd_recovery_mode=recover snapd_recovery_system=1234 console=ttyS0 console=tty1 panic=-1",
						"snapd_recovery_mode=factory-reset snapd_recovery_system=1234 console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: kernelNewRecovery,
				},
			})
			c.Check(params.RecoveryBootChains, DeepEquals, []boot.BootChain{
				{
					BrandID:        "my-brand",
					Model:          "my-model-uc20",
					Grade:          "dangerous",
					ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
					AssetChain:     recoveryAssetChain,
					Kernel:         "pc-kernel",
					KernelRevision: "1",
					KernelCmdlines: []string{
						"snapd_recovery_mode=recover snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
						"snapd_recovery_mode=factory-reset snapd_recovery_system=20200825 console=ttyS0 console=tty1 panic=-1",
					},
					KernelBootFile: kernelOldRecovery,
				},
			})
		default:
			c.Errorf("unexpected additional call to ResealKeyForBootChains (call # %d)", resealKeysCalls)
		}

		return nil
	})
	defer restore()

	// here we don't have unasserted kernels so just set
	// expectReseal to false as it doesn't matter;
	// the behavior with unasserted kernel is tested in
	// boot_test.go specific tests
	opts := boot.ResealKeyToModeenvOptions{ExpectReseal: false}
	err = boot.ResealKeyToModeenv(rootdir, modeenv, opts, nil)
	c.Assert(err, IsNil)
	c.Assert(resealKeysCalls, Equals, 1)
}

func (s *sealSuite) TestResealKeyToModeenvWithTryModelOldBootChain(c *C) {
	s.testResealKeyToModeenvWithTryModel(c, "bootx64.efi", "grubx64.efi")
}

func (s *sealSuite) TestResealKeyToModeenvWithTryModelNewBootChain(c *C) {
	s.testResealKeyToModeenvWithTryModel(c, "ubuntu:shimx64.efi", "ubuntu:grubx64.efi")
}

func (s *sealSuite) TestWithBootChains(c *C) {
	rootdir := c.MkDir()
	dirs.SetRootDir(rootdir)
	defer dirs.SetRootDir("")

	model := boottest.MakeMockUC20Model()

	modeenv := &boot.Modeenv{
		Mode: "run",

		// no recovery systems to keep things relatively short
		//
		CurrentTrustedRecoveryBootAssets: boot.BootAssetsMap{
			"grubx64.efi": []string{"grub-hash"},
			"bootx64.efi": []string{"shim-hash"},
		},

		CurrentTrustedBootAssets: boot.BootAssetsMap{
			"grubx64.efi": []string{"run-grub-hash"},
		},

		CurrentKernels: []string{"pc-kernel_500.snap"},

		CurrentKernelCommandLines: boot.BootCommandLines{
			"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
		},
		Model:          model.Model(),
		BrandID:        model.BrandID(),
		Grade:          string(model.Grade()),
		ModelSignKeyID: model.SignKeyID(),
	}

	c.Assert(modeenv.WriteTo(dirs.GlobalRootDir), IsNil)

	err := createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-seed"))
	c.Assert(err, IsNil)

	err = createMockGrubCfg(filepath.Join(rootdir, "run/mnt/ubuntu-boot"))
	c.Assert(err, IsNil)

	// mock asset cache
	boottest.MockAssetsCache(c, rootdir, "grub", []string{
		"run-grub-hash",
		"grub-hash",
		"shim-hash",
	})

	restore := boot.MockSeedReadSystemEssential(func(seedDir, label string, essentialTypes []snap.Type, tm timings.Measurer) (*asserts.Model, []*seed.Snap, error) {
		return model, []*seed.Snap{mockKernelSeedSnap(snap.R(1)), mockGadgetSeedSnap(c, nil)}, nil
	})
	defer restore()

	var chains boot.BootChains
	err = boot.WithBootChains(func(ch boot.BootChains) error {
		chains = ch
		return nil
	}, device.SealingMethodTPM)
	c.Assert(err, IsNil)

	c.Check(chains, DeepEquals, boot.BootChains{
		RunModeBootChains: []boot.BootChain{
			{
				BrandID:        "my-brand",
				Model:          "my-model-uc20",
				Grade:          "dangerous",
				ModelSignKeyID: "Jv8_JiHiIzJVcO9M55pPdqSDWUvuhfDIBJUS-3VW7F_idjix7Ffn5qMxB21ZQuij",
				AssetChain: []boot.BootAsset{
					{
						Role:   bootloader.RoleRecovery,
						Name:   "bootx64.efi",
						Hashes: []string{"shim-hash"},
					},
					{
						Role:   bootloader.RoleRecovery,
						Name:   "grubx64.efi",
						Hashes: []string{"grub-hash"},
					},
					{
						Role:   bootloader.RoleRunMode,
						Name:   "grubx64.efi",
						Hashes: []string{"run-grub-hash"},
					},
				},
				Kernel:         "pc-kernel",
				KernelRevision: "500",
				KernelCmdlines: []string{
					"snapd_recovery_mode=run console=ttyS0 console=tty1 panic=-1",
				},
				KernelBootFile: bootloader.BootFile{
					Path: "kernel.efi",
					Snap: filepath.Join(rootdir, "var/lib/snapd/snaps/pc-kernel_500.snap"),
					Role: bootloader.RoleRunMode,
				},
			},
		},
		RoleToBlName: map[bootloader.Role]string{
			bootloader.RoleRunMode:  "grub",
			bootloader.RoleRecovery: "grub",
		},
	})
}
