package storageidentity

import "fmt"

// BindingPath keeps the v4 optional configured_path fact wire compatible.
// A helper response cannot substitute even a case-equivalent route: the
// validated configured spelling is the only address saved at first binding.
func (response HelperResponse) BindingPath(request HelperRequest) (string, error) {
	if response.ConfiguredPath == "" {
		return request.Path, nil
	}
	if !request.SelectSpelling || request.Operation == "enumerate" || response.ConfiguredPath != request.Path {
		return "", fmt.Errorf("unexpected configured spelling")
	}
	return request.Path, nil
}
