package daemon

import (
	"bytes"
	"io"
	"net/http"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/signtool"
	"github.com/snapcore/snapd/overlord/auth"
)

var (
	signCmd = &Command{
		Path:        "/v2/sign",
		POST:        doSign,
		WriteAccess: authenticatedAccess{},
	}
)

func doSign(c *Command, r *http.Request, user *auth.UserState) Response {
	body, err := io.ReadAll(r.Body)

	if err != nil {
		return BadRequest("cannot read request body: %v", err)
	}

	state := c.d.overlord.State()
	state.Lock()
	defer state.Unlock()

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
