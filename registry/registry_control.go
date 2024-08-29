package registry

// TODO: Add comments to all exported names

type RegistryControl struct {
	OperatorID string
	views      []DelegatedView
}

type DelegatedView struct {
	Name string
}

func NewRegistryControl(operatorID string, views []interface{}) (*RegistryControl, error) {
	registryControl := &RegistryControl{
		OperatorID: operatorID,
		views:      make([]DelegatedView, len(views)),
	}

	for _, view := range views {
		// TODO: handle/propagate err
		delegatedView, _ := newDelegatedView(view.(map[string]interface{}))

		registryControl.views = append(registryControl.views, *delegatedView)
	}

	return registryControl, nil
}

func newDelegatedView(view map[string]interface{}) (*DelegatedView, error) {
	// TODO: validate "name" exists
	// TODO: validate the path pattern
	// 	& that the registry exists
	//		cache the checks?
	return &DelegatedView{Name: view["name"].(string)}, nil
}
