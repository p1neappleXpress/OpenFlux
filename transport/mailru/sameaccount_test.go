package mailru

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// chatEntry is one broadcast the server relays: user is the sending connection's editor id, original the account's.
func chatEntry(user, original string, payload []byte) []byte {
	msg := chatDataTag + base64.StdEncoding.EncodeToString(payload)
	return []byte(fmt.Sprintf(`42["message",{"type":"message","messages":[{"docid":"d","message":%q,"time":1,"user":%q,"useridoriginal":%q,"username":"n"}]}]`, msg, user, original))
}

// Both ends signed in as one account: every connection has the same useridoriginal, so the exit's frames can only be
// told from our own echo by the per-connection id.
func TestSameAccountPeerFramesAreNotTakenForOurEcho(t *testing.T) {
	tr := NewMailruDocsTransport("https://cloud.mail.ru/public/aaaa/bbbb", transport.DefaultConfig())
	tr.ourIDs.Store("uid1", struct{}{}) // the account's id, as before
	tr.ourConns.Store("uid1107", struct{}{})

	own := decodeChatFrames(chatEntry("uid1107", "uid1", []byte("echo")), tr.isOwnFrame)
	if len(own) != 0 {
		t.Fatalf("our own echo was delivered: %q", own)
	}
	peer := decodeChatFrames(chatEntry("uid1108", "uid1", []byte("from the exit")), tr.isOwnFrame)
	if len(peer) != 1 || string(peer[0]) != "from the exit" {
		t.Fatalf("a frame from the account's other connection was dropped: %q", peer)
	}
}

// With no connection id learned (an older server's auth reply) the account-level id is the filter, as it always was.
func TestFallsBackToTheAccountIDWhenTheConnectionIDIsUnknown(t *testing.T) {
	tr := NewMailruDocsTransport("https://cloud.mail.ru/public/aaaa/bbbb", transport.DefaultConfig())
	tr.ourIDs.Store("anon-7", struct{}{})
	if got := decodeChatFrames(chatEntry("anon-7", "anon-7", []byte("echo")), tr.isOwnFrame); len(got) != 0 {
		t.Fatalf("an anonymous echo was delivered: %q", got)
	}
	if got := decodeChatFrames(chatEntry("anon-9", "anon-9", []byte("peer")), tr.isOwnFrame); len(got) != 1 {
		t.Fatalf("an anonymous peer was dropped: %q", got)
	}
}

func TestLearnConnIDTakesTheParticipantWhoseConnectionIDIsOurSid(t *testing.T) {
	tr := NewMailruDocsTransport("https://cloud.mail.ru/public/aaaa/bbbb", transport.DefaultConfig())
	s := &DocSession{sid: "SID-B"}
	auth := []byte(`42["message",{"type":"auth","result":1,"participants":[` +
		`{"id":"uid1107","idOriginal":"uid1","connectionId":"SID-A"},` +
		`{"id":"uid1108","idOriginal":"uid1","connectionId":"SID-B"}]}]`)
	tr.learnConnID(s, auth)
	if _, ok := tr.ourConns.Load("uid1108"); !ok {
		t.Fatal("our connection id was not learned")
	}
	if _, ok := tr.ourConns.Load("uid1107"); ok {
		t.Fatal("another connection's id was taken for ours")
	}
	tr.learnConnID(&DocSession{}, auth) // no sid: nothing is learned, nothing panics
}
