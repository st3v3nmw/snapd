package asserts

type RequestMessage struct {
	assertionBase
}

func assembleRequestMessage(assert assertionBase) (Assertion, error) {
	return &RequestMessage{assertionBase: assert}, nil
}

type ResponseMessage struct {
	assertionBase
}

func assembleResponseMessage(assert assertionBase) (Assertion, error) {
	return &ResponseMessage{assertionBase: assert}, nil
}
