package script

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/dop251/goja"

	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
	"github.com/p1neappleXpress/OpenFlux/transport/yandex"
)

// Parity between a native transport and its JS port. The pure seams (message
// builders, frame parsers) are run through BOTH implementations and compared
// byte for byte: that is what keeps a client on one and an exit on the other
// speaking the same wire. See docs/plans/2026-10-04-native-to-js-parity.md for
// what is covered and what is only reviewed.

// loadJS evaluates a shipped script's source under the restricted inspect API
// (its top level defines functions and state; nothing here touches the net)
// and returns the runtime so a test can call the script's own functions.
func loadJS(t *testing.T, path string) *goja.Runtime {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	registerInspectAPI(vm)
	if _, err := vm.RunString(string(src)); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return vm
}

func jsCall(t *testing.T, vm *goja.Runtime, name string, args ...interface{}) goja.Value {
	t.Helper()
	fn, ok := goja.AssertFunction(vm.Get(name))
	if !ok {
		t.Fatalf("the script has no function %s", name)
	}
	in := make([]goja.Value, len(args))
	for i, a := range args {
		in[i] = vm.ToValue(a)
	}
	out, err := fn(goja.Undefined(), in...)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, args, err)
	}
	return out
}

// What the JS transport hands to the engine for a chat broadcast (emit) equals
// what the native one delivers: only our-tagged frames from other participants,
// base64-decoded, in order, with the server's echo of our own chat and bad
// base64 skipped. This is what lets a JS client and a native exit (or the
// reverse) interoperate over the chat data path.
func TestParityMailruChatData(t *testing.T) {
	cases := []struct {
		frame, own string
	}{
		// a peer's tagged packet
		{`42["message",{"type":"message","messages":[{"message":"OFX1aGVsbG8=","useridoriginal":"peer"}]}]`, "me"},
		// the server's echo of our own chat: skipped
		{`42["message",{"type":"message","messages":[{"message":"OFX1aGVsbG8=","useridoriginal":"me"}]}]`, "me"},
		// a human's chat (no tag) mixed with a tagged peer frame
		{`42["message",{"type":"message","messages":[{"message":"hi there","useridoriginal":"peer"},{"message":"OFX1b25l","useridoriginal":"peer"}]}]`, "me"},
		// several tagged entries, one not base64 (skipped)
		{`42["message",{"type":"message","messages":[{"message":"OFX1YQ==","useridoriginal":"peer"},{"message":"OFX1!!bad","useridoriginal":"peer"},{"message":"OFX1Yg==","useridoriginal":"peer"}]}]`, "me"},
		// empty ownID: nothing is treated as self-echo
		{`42["message",{"type":"message","messages":[{"message":"OFX1dHdv","useridoriginal":"me"}]}]`, ""},
		// not a chat broadcast
		{`42["message",{"type":"auth","result":1}]`, "me"},
		{`3`, "me"},
	}
	for _, c := range cases {
		vm := loadJS(t, "js/mailru.js")
		if err := vm.Set("ownID", c.own); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := vm.Set("emit", func(call goja.FunctionCall) goja.Value {
			var b []byte
			_ = vm.ExportTo(call.Argument(0), &b)
			got = append(got, string(b))
			return goja.Undefined()
		}); err != nil {
			t.Fatal(err)
		}
		jsCall(t, vm, "handleMessage", c.frame)

		var want []string
		for _, b := range mailru.DecodeChatData([]byte(c.frame), c.own) {
			want = append(want, string(b))
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("frame %q own %q: js delivered %q, native %q", c.frame, c.own, got, want)
		}
	}
}

func TestParityYandexSaveChanges(t *testing.T) {
	vm := loadJS(t, "js/yandex.js")
	ids := []string{"1234567890", "12345678901234", "short", "x", ""}
	// seq goes through byte(): the wrap at 256 must match.
	seqs := []int{1, 2, 3, 5, 100, 253, 254, 255, 256, 257, 511, 1000}
	for _, id := range ids {
		for _, excel := range []bool{false, true} {
			for _, seq := range seqs {
				want := string(yandex.BuildSaveChanges(id, excel, seq))
				got := jsCall(t, vm, "buildSaveChanges", id, excel, seq).String()
				if got != want {
					t.Fatalf("user %q excel=%v seq=%d:\n js : %s\n go : %s", id, excel, seq, got, want)
				}
			}
		}
	}
}

func TestParityYandexIsExcel(t *testing.T) {
	vm := loadJS(t, "js/yandex.js")
	for _, ft := range []string{"xlsx", "XLSX", ".xlsx", "xls", "xlsm", "csv", "docx", "pptx", "", "txt", ".CSV"} {
		// The Go side is unexported; its rule is "fileType minus a leading dot, lower-cased, is one of xlsx/xls/xlsm/csv".
		want := false
		switch lower(trimDot(ft)) {
		case "xlsx", "xls", "xlsm", "csv":
			want = true
		}
		if got := jsCall(t, vm, "isExcelFile", ft).ToBoolean(); got != want {
			t.Errorf("isExcelFile(%q) = %v, want %v", ft, got, want)
		}
	}
}

func trimDot(s string) string {
	if len(s) > 0 && s[0] == '.' {
		return s[1:]
	}
	return s
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestParityYandexExtractBase64(t *testing.T) {
	vm := loadJS(t, "js/yandex.js")
	frames := []string{
		`42["message",{"type":"cursor","cursor":"18;aGVsbG8="}]`,
		`42["message",{"type":"cursor","cursor":"35;QUJD"}]`,
		`42["message",{"type":"saveChanges","excelAdditionalInfo":"d29ybGQ=","isExcel":true}]`,
		`42["message",{"type":"saveChanges","changes":"[]","excelAdditionalInfo":"{\"UserId\":\"1\"}"}]`,
		`42["message",{"type":"saveChanges","changes":"[]"}]`,
		`42["message",{"type":"authChanges"}]`,
		`2`,
		``,
	}
	for _, f := range frames {
		want := yandex.ExtractBase64(f)
		got := jsCall(t, vm, "extractBase64", f).String()
		if got != want {
			t.Errorf("frame %q: js %q, go %q", f, got, want)
		}
	}
}

// jsJSON runs JSON.stringify over a value the script built, so it can be
// compared with Go's encoding/json output.
func jsJSON(t *testing.T, vm *goja.Runtime, v goja.Value) string {
	t.Helper()
	if err := vm.Set("__v", v); err != nil {
		t.Fatal(err)
	}
	out, err := vm.RunString("JSON.stringify(__v)")
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestParityBoardsModifyObjects(t *testing.T) {
	vm := loadJS(t, "js/boards.js")
	cases := []struct {
		b64, id             string
		x, y                int
		creator, participnt string
	}{
		{"aGVsbG8=", "0123456789abcdef0123456789abcdef", 0, 0, "creatorhash", "participanthash"},
		{"AAECAwQFBgcICQ==", "ffffffffffffffffffffffffffffffff", 1999, 1199, "c", "p"},
		{"", "00", 17, 1000, "", ""},
	}
	for _, c := range cases {
		want := string(yandex.BoardsModifyObjects(c.b64, c.id, c.x, c.y, c.creator, c.participnt))
		obj := jsCall(t, vm, "buildModifyObjects", c.b64, c.id, c.x, c.y, c.creator, c.participnt)
		wrapped, err := vm.RunString("(function (o) { return ['dashboard', o]; })")
		if err != nil {
			t.Fatal(err)
		}
		fn, _ := goja.AssertFunction(wrapped)
		arr, err := fn(goja.Undefined(), obj)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsJSON(t, vm, arr); got != want {
			t.Errorf("case %+v:\n js : %s\n go : %s", c, got, want)
		}
	}
}

func TestParityBoardsModifyObjectsPayloads(t *testing.T) {
	events := []string{
		// a peer's packet
		`{"name":"peer","objects":[{"_attributes_":{"id":"a","value":"aGVsbG8=","creatorHash":"other","type":"textbox"},"hash":"a","mxGeometry":[{"_attributes_":{"x":"1"}}]}]}`,
		// our own echo by creator, by participant, by name
		`{"name":"peer","objects":[{"_attributes_":{"value":"aGVsbG8=","creatorHash":"mine"},"hash":"a"}]}`,
		`{"name":"peer","objects":[{"_attributes_":{"value":"aGVsbG8=","creatorHash":"myuser"},"hash":"a"}]}`,
		`{"name":"me","objects":[{"_attributes_":{"value":"aGVsbG8=","creatorHash":"other"},"hash":"a"}]}`,
		// several objects, one not base64, one empty value, one without attributes
		`{"objects":[{"_attributes_":{"value":"b25l","creatorHash":"x"},"hash":"1"},{"_attributes_":{"value":"!!!"},"hash":"2"},{"_attributes_":{"value":""},"hash":"3"},{"hash":"4"},{"_attributes_":{"value":"dHdv"},"hash":"5","mxGeometry":[]}]}`,
		`{}`,
		`{"objects":[]}`,
	}
	vm := loadJS(t, "js/boards.js")
	for _, ev := range events {
		wantP, wantD := yandex.ModifyObjectsPayloads([]byte(ev), "mine", "myuser", "me")

		var data interface{}
		if err := json.Unmarshal([]byte(ev), &data); err != nil {
			t.Fatal(err)
		}
		res := jsCall(t, vm, "modifyObjectsPayloads", data, "mine", "myuser", "me").ToObject(vm)

		var gotP []string
		pl := res.Get("payloads").ToObject(vm)
		for i := 0; i < int(pl.Get("length").ToInteger()); i++ {
			var b []byte
			_ = vm.ExportTo(pl.Get(fmt.Sprint(i)), &b)
			gotP = append(gotP, string(b))
		}
		var wantPS []string
		for _, b := range wantP {
			wantPS = append(wantPS, string(b))
		}
		if fmt.Sprint(gotP) != fmt.Sprint(wantPS) {
			t.Errorf("event %s: js payloads %q, go %q", ev, gotP, wantPS)
		}

		// The drop entries: the same objects (JSON-equal; key order is the
		// server's in JS and sorted in Go).
		var gotD, wantDJ interface{}
		_ = json.Unmarshal([]byte(jsJSON(t, vm, res.Get("drop"))), &gotD)
		raw, _ := json.Marshal(wantD)
		_ = json.Unmarshal(raw, &wantDJ)
		if len(wantD) == 0 && (gotD == nil || len(gotD.([]interface{})) == 0) {
			continue
		}
		if !reflect.DeepEqual(gotD, wantDJ) {
			t.Errorf("event %s: js drop %v, go %v", ev, gotD, wantDJ)
		}
	}
}
