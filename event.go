package actioncable

import (
	"encoding/json"
)

type Event struct {
	Type       string             `json:"type"`
	Message    json.RawMessage    `json:"message"`
	Reason     json.RawMessage    `json:"reason"`
	Reconnect  json.RawMessage    `json:"reconnect"`
	Data       json.RawMessage    `json:"data"`
	Identifier *ChannelIdentifier `json:"identifier"`
}

func (e *Event) GetReconnect(reconnect *bool) bool {
	return e.getField(e.Reconnect, reconnect)
}

func (e *Event) GetReason(reason *string) bool {
	return e.getField(e.Reason, reason)
}

func (e *Event) getField(field json.RawMessage, val interface{}) bool {
	if len(field) == 0 {
		return false
	}
	err := json.Unmarshal(field, val)
	return err != nil
}

func NewNilEvent() *Event {
	return &Event{}
}
