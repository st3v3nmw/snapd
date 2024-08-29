package asserts

import (
	"github.com/snapcore/snapd/registry"
)

// TODO: Add comments to all exported names

type RegistryControl struct {
	assertionBase
	registryControl *registry.RegistryControl
}

// signKey returns the underlying public key of
// the requested registry-control assertion
func (rgctrl *RegistryControl) signKey() PublicKey {
	// TODO
	// it's hard to do this at this layer since
	// we don't have access to the assertions datababse
	return nil
}

func assembleRegistryControl(assert assertionBase) (Assertion, error) {
	operatorID := assert.HeaderString("operator-id")

	// TODO: check views exist
	// basically do all the necessary validations
	views := assert.Header("views")

	registryControl, err := registry.NewRegistryControl(operatorID, views.([]interface{}))
	if err != nil {
		return nil, err
	}

	return &RegistryControl{
		assertionBase:   assert,
		registryControl: registryControl,
	}, nil
}
