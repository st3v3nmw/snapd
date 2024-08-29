package daemon

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/signtool"
	"github.com/snapcore/snapd/features"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/state"
)

var (
	signCmd = &Command{
		Path:        "/v2/sign",
		POST:        doSign,
		WriteAccess: authenticatedAccess{},
	}
)

func doSign(c *Command, r *http.Request, user *auth.UserState) Response {
	state := c.d.overlord.State()
	state.Lock()
	defer state.Unlock()

	if err := validateRegistryControlFeatureFlag(state); err != nil {
		return BadRequest("%v", err)
	}

	body, err := io.ReadAll(r.Body)

	if err != nil {
		return BadRequest("cannot read request body: %v", err)
	}

	deviceMgr := c.d.overlord.DeviceManager()

	devKeyID, _ := deviceMgr.KeyID()
	keypairMgr, _ := deviceMgr.KeyMgr()

	signOpts := signtool.Options{
		KeyID:     devKeyID,
		Statement: body,
	}

	encodedAssert, err := signtool.Sign(&signOpts, keypairMgr)
	if err != nil {
		return BadRequest("cannot sign assertion: %v", err)
	}

	outBuf := bytes.NewBuffer(nil)
	enc := asserts.NewEncoder(outBuf)

	err = enc.WriteEncoded(encodedAssert)
	if err != nil {
		return BadRequest("cannot sign assertion: %v", err)
	}

	return SyncResponse(outBuf.String())
}

func validateRegistryControlFeatureFlag(st *state.State) *apiError {
	tr := config.NewTransaction(st)
	enabled, err := features.Flag(tr, features.RegistryControl)
	if err != nil && !config.IsNoOption(err) {
		return InternalError(fmt.Sprintf("internal error: cannot check registry-control feature flag: %s", err))
	}

	if !enabled {
		_, confName := features.RegistryControl.ConfigOption()
		return BadRequest(fmt.Sprintf(`"registry-control" feature flag is disabled: set '%s' to true`, confName))
	}
	return nil
}
