package sub

import "encoding/json"

func jsonUnmarshal(raw json.RawMessage, v interface{}) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
