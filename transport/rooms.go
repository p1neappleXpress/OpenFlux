package transport

import (
	"encoding/base64"
	"encoding/json"
	"errors"
)

// Room lists are what a room-based carrier (cups.online, Avito Code) hands
// the client as its --url: the rooms the exit created, base64url of JSON.
//
// The first format was a bare array of room ids, and the two ends spread the
// tunnel over the rooms without any order. A list in the second format,
// {"v":2,"rooms":[...]}, says the node runs a stripe over them: one lane per
// room, sequenced and acked (StripedTransport). Both ends must agree, and the
// list is what they share, so the format decides: a node or client given an
// old list keeps the old wire and still talks to old peers, and an old build
// given a new list fails to parse it instead of exchanging garbage.

// stripedRoomsVersion marks a room list whose node stripes over its rooms.
const stripedRoomsVersion = 2

type roomListV2 struct {
	V     int      `json:"v"`
	Rooms []string `json:"rooms"`
}

// PackRooms packs room ids for --url: the striped format when striped, the
// old bare array otherwise.
func PackRooms(ids []string, striped bool) string {
	var raw []byte
	if striped {
		raw, _ = json.Marshal(roomListV2{V: stripedRoomsVersion, Rooms: ids})
	} else {
		raw, _ = json.Marshal(ids)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// UnpackRooms reads a list PackRooms made, either format.
func UnpackRooms(s string) (ids []string, striped bool, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false, err
	}
	if len(raw) > 0 && raw[0] == '{' {
		var v roomListV2
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false, err
		}
		if v.V != stripedRoomsVersion || len(v.Rooms) == 0 {
			return nil, false, errors.New("room list: unknown format")
		}
		return v.Rooms, true, nil
	}
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, false, err
	}
	return ids, false, nil
}
