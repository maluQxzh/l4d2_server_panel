package agent

import "errors"

type jobFailureResultProvider interface {
	JobFailureResult() map[string]any
}

func jobFailureResult(err error) map[string]any {
	var provider jobFailureResultProvider
	if err != nil && errors.As(err, &provider) {
		return provider.JobFailureResult()
	}
	return nil
}
