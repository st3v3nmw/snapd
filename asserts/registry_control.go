package asserts

import (
	"github.com/snapcore/snapd/registry"
)

// TODO: Add comments to all exported names

type RegistryControl struct {
	assertionBase
	registryControl *registry.RegistryControl
}

func assembleRegistryControl(assert assertionBase) (Assertion, error) {
	operatorID := assert.HeaderString("operator-id")

	// TODO: check views exist
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
