package repositories

type PermissionDeniedOrNotFoundError struct {
	Err          error
	ResourceType string
}

func (e PermissionDeniedOrNotFoundError) Error() string {
	msg := "not found"
	if e.ResourceType != "" {
		msg = e.ResourceType + " " + msg
	}
	if e.Err != nil {
		msg = msg + ": " + e.Err.Error()
	}
	return msg
}

func (e PermissionDeniedOrNotFoundError) Unwrap() error {
	return e.Err
}
